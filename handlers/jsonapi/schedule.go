// handlers/jsonapi/schedule.go — the scheduled-send drain + the /v1/scheduled API.
//
// DESIGN (mirrors the snooze re-delivery scheduler + Talk's wave-46 scheduled
// send): persistence lives in scheduleStore (durable KV), a single background
// goroutine polls on a fixed interval, and on boot it runs an immediate catch-up
// pass so anything overdue while the process was down fires promptly. Delivery is
// AT-LEAST-ONCE: a record is deleted only AFTER a successful send, so a crash
// mid-send re-fires it on the next poll (a rare duplicate) rather than silently
// dropping the mail. The stable record id is the dedup key if a downstream ever
// wants exactly-once.
//
// Every fire rebuilds the MIME via api.BuildMIMEMessage and sends via SMTP —
// the SAME engine as an immediate send — so the wave-49 header-injection guard
// (validateHeaderValue) and wave-44 cid: inline handling run at ACTUAL send time,
// not merely at schedule time.
package jsonapi

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// scheduleHorizon bounds how far in the future a send may be scheduled. A far-off
// or absurd sendAt is rejected (400): it would pin a credential in storage for an
// unreasonable time and is almost always a client bug (e.g. milliseconds instead
// of seconds). One year is generous for legitimate send-later use.
const scheduleHorizon = 365 * 24 * time.Hour

// oauthScheduleHorizon is the tighter cap applied when the account authenticates
// with a short-lived OAuth access token (brokered Gmail/Outlook, or a session
// OAuth login). Beyond this the captured token would be expired at fire time, so
// the schedule is refused up front (400) rather than accepted and silently failed.
// Kept modest but useful for same-day / next-morning send-later.
const oauthScheduleHorizon = 12 * time.Hour

// schedulePollInterval is how often the drain scans for due sends. Send-later
// does not need second-precision; a coarse poll keeps the load negligible while
// bounding worst-case lateness to one interval.
var schedulePollInterval = 30 * time.Second

// maxSendAttempts bounds the at-least-once retry loop for a record whose SMTP SEND
// keeps failing. A transient failure (network blip, greylisting, momentary auth
// hiccup) should be retried, but a PERMANENT failure — revoked credentials after a
// password change, a hard 5xx recipient reject — must NOT be re-dialed forever:
// that pins the encrypted credential in storage, burns a per-account quota slot
// indefinitely, and hammers the SMTP server every poll. After this many failed
// send attempts the record is abandoned (deleted, logged) — fail-closed rather
// than an unbounded loop. Generous enough that legitimate transient outages
// recover well within the budget at the poll cadence.
var maxSendAttempts = 20

// scheduleSMTPFactory builds an SMTP sender from a record's persisted transport
// fields. Package var so the drain can be tested without a live SMTP server.
var scheduleSMTPFactory = func(rec *scheduledSend) smtpSender {
	var cl *api.SMTPClient
	if rec.SMTPUseOAuth {
		cl = api.NewSMTPClientOAuth(rec.SMTPHost, rec.SMTPPort, rec.From, rec.Secret, rec.OAuthMech, rec.UseSTARTTLS)
	} else {
		cl = api.NewSMTPClient(rec.SMTPHost, rec.SMTPPort, rec.From, rec.Secret, rec.UseSTARTTLS)
	}
	if cl != nil {
		cl.SetInsecureSkipVerify(rec.InsecureSkip)
	}
	return cl
}

// scheduler owns the drain goroutine + the store. One per process.
type scheduler struct {
	store   *scheduleStore
	stop    chan struct{}
	done    chan struct{} // closed when the drain goroutine has fully exited
	once    sync.Once
	started bool // set under once.Do when the drain goroutine is actually launched
}

func newScheduler(store *scheduleStore) *scheduler {
	return &scheduler{store: store, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the drain goroutine: an immediate catch-up pass (restart-safe),
// then a poll loop. Safe to call once; a nil scheduler or nil store is a no-op so
// standalone builds without a KV configured simply never schedule.
func (s *scheduler) Start() {
	if s == nil || s.store == nil {
		return
	}
	s.once.Do(func() {
		s.started = true
		go func() {
			defer close(s.done) // signal Stop that the drain has fully wound down
			s.drainDue()        // boot catch-up: fire anything already overdue
			t := time.NewTicker(schedulePollInterval)
			defer t.Stop()
			for {
				select {
				case <-s.stop:
					return
				case <-t.C:
					s.drainDue()
				}
			}
		}()
	})
}

// Stop halts the drain goroutine and BLOCKS until the in-flight drain pass (if any)
// has returned, so callers/tests can safely tear down the underlying KV afterward
// without racing a mid-drain read/write. Idempotent.
func (s *scheduler) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	// Wait for the drain goroutine to exit — only if it was actually launched. If
	// Start never ran (nil store / never called), `done` is never closed, so we must
	// not block on it. `started` is written under once.Do before the goroutine spawns
	// and Stop is only meaningfully called after Start returns, so this read is safe.
	if s.started {
		<-s.done
	}
}

// drainDue sends every record due at now. A send failure leaves the record in
// place to be retried on the next pass (at-least-once); a success deletes it.
func (s *scheduler) drainDue() {
	due, err := s.store.listAllDue(time.Now())
	if err != nil {
		log.Printf("jsonapi: scheduled-send drain: list due: %v", err)
		return
	}
	for _, rec := range due {
		s.fire(rec)
	}
}

// fire builds the MIME (re-running the header-injection guard + cid: handling) and
// sends it, then deletes the record on success. Errors are logged and the record
// is left for the next pass.
func (s *scheduler) fire(rec *scheduledSend) {
	if err := s.store.decryptSecret(rec); err != nil {
		log.Printf("jsonapi: scheduled-send %s: decrypt secret: %v", rec.ID, err)
		return
	}

	raw, rcpts, err := buildScheduledMIME(rec)
	if err != nil {
		// A permanent build failure (e.g. a header-injection attempt that somehow
		// slipped past the schedule-time check) must NOT loop forever. Drop it.
		log.Printf("jsonapi: scheduled-send %s: build failed, dropping: %v", rec.ID, err)
		_ = s.store.Delete(rec.Account, rec.ID)
		return
	}

	sender := scheduleSMTPFactory(rec)
	if sender == nil {
		log.Printf("jsonapi: scheduled-send %s: no SMTP sender", rec.ID)
		return
	}
	if err := sender.SendRawMessage(rcpts, raw); err != nil {
		// A send failure is retried on the next poll (at-least-once), but NOT forever:
		// bump the attempt counter and abandon the record once it exhausts the budget,
		// so a permanently-failing send (revoked creds, hard 5xx) cannot loop and pin
		// the credential/quota slot indefinitely. Fail-closed on a persist error too:
		// if we cannot record the attempt, drop it rather than risk an unbounded loop
		// with a counter that never advances.
		rec.Attempts++
		if rec.Attempts >= maxSendAttempts {
			log.Printf("jsonapi: scheduled-send %s: SMTP send failed %d times, abandoning: %v", rec.ID, rec.Attempts, err)
			_ = s.store.Delete(rec.Account, rec.ID)
			return
		}
		if perr := s.store.Put(rec); perr != nil {
			log.Printf("jsonapi: scheduled-send %s: cannot persist retry counter (%v), abandoning after send failure: %v", rec.ID, perr, err)
			_ = s.store.Delete(rec.Account, rec.ID)
			return
		}
		log.Printf("jsonapi: scheduled-send %s: SMTP send failed (attempt %d/%d, will retry): %v", rec.ID, rec.Attempts, maxSendAttempts, err)
		return
	}
	// Sent: delete so it does not re-fire. This delete-after-send ordering is what
	// makes delivery at-least-once (prefer a rare duplicate over a silent drop).
	if err := s.store.Delete(rec.Account, rec.ID); err != nil {
		log.Printf("jsonapi: scheduled-send %s: sent but delete failed (may duplicate): %v", rec.ID, err)
	}
}

// buildScheduledMIME assembles the record into a raw MIME message + RCPT list,
// reusing api.BuildMIMEMessage — so validateHeaderValue (wave-49) and the cid:
// inline-attachment handling (wave-44) both run here at fire time.
func buildScheduledMIME(rec *scheduledSend) (raw []byte, rcpts []string, err error) {
	plain := rec.Text
	if plain == "" && rec.HTML != "" {
		plain = stripHTMLForPlain(rec.HTML)
	}
	raw, err = api.BuildMIMEMessage(api.MIMEMessageOptions{
		From:        rec.From,
		To:          rec.To,
		Cc:          rec.Cc,
		Subject:     rec.Subject,
		InReplyTo:   rec.InReplyTo,
		PlainBody:   plain,
		HTMLBody:    rec.HTML,
		Attachments: rec.Attachments,
	})
	if err != nil {
		return nil, nil, err
	}
	for _, a := range api.ParseAddressField(rec.To) {
		rcpts = append(rcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(rec.Cc) {
		rcpts = append(rcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(rec.Bcc) {
		rcpts = append(rcpts, a.Email)
	}
	return raw, rcpts, nil
}

// parseFutureSendAt validates a caller-supplied sendAt: it must parse as RFC3339,
// be in the FUTURE, and fall WITHIN scheduleHorizon. A past or absurd time is a
// 400 (returned as a plain error the handler surfaces) — never silently clamped.
func parseFutureSendAt(raw string) (time.Time, error) {
	when, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("sendAt must be an RFC3339 timestamp")
	}
	now := time.Now()
	// A tiny grace (a few seconds) tolerates clock skew / in-flight latency without
	// accepting a genuinely past time.
	if when.Before(now.Add(-5 * time.Second)) {
		return time.Time{}, fmt.Errorf("sendAt must be in the future")
	}
	if when.After(now.Add(scheduleHorizon)) {
		return time.Time{}, fmt.Errorf("sendAt is too far in the future (max 1 year)")
	}
	return when, nil
}

// scheduleSend persists a compose payload as a future scheduled send and returns
// 202 Accepted. It captures the account's SMTP transport (host/port/secret) NOW,
// encrypted at rest by the store, so the drain can reconnect + send at sendAt with
// no live request context. Rejects a non-future/absurd sendAt (400) and enforces
// the per-account pending quota (429).
//
// `from` is the already-GATED sender (handleSend → sendFrom): the authenticated
// mailbox, or one of its REGISTERED send-as identities. The record is nonetheless
// OWNED by the authenticated mailbox — the store keys every read/list/cancel on
// Account, so ownership must never move to an alias (a scheduled send the owner
// could neither see nor cancel, in a namespace that is not theirs). From (the
// header the message fires with) and Account (who owns the record) are therefore
// resolved separately.
func (h *Handler) scheduleSend(c *fiber.Ctx, body composeBody, plain string, atts []api.OutgoingAttachment, from string) error {
	if h.schedule == nil {
		return fail(c, fiber.StatusNotImplemented, "scheduled send is not enabled")
	}
	when, err := parseFutureSendAt(body.SendAt)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	owner := strings.TrimSpace(h.fromEmail(c))
	if owner == "" || strings.TrimSpace(from) == "" {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}

	// Capture the SMTP transport (including the auth secret) for a later reconnect.
	// This reuses the SAME broker/session/oauth credential logic as an immediate
	// send — Transport() just snapshots what smtpClient(c) already built.
	tr, err := h.smtpTransport(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "failed to prepare mail transport")
	}

	// An OAuth transport carries a SHORT-LIVED access token. Persisting it for a
	// far-future send would just fail at fire time (expired token) — a silent-ish
	// drop after pointless retries. Refuse honestly up front rather than accept a
	// send that cannot succeed. Password (plain) auth has no such horizon.
	if tr.UseOAuth && when.After(time.Now().Add(oauthScheduleHorizon)) {
		return fail(c, fiber.StatusBadRequest,
			"sendAt is beyond the horizon supported for this account's credentials")
	}

	rec := &scheduledSend{
		ID:           newScheduleID(),
		Account:      owner, // record ownership: always the authenticated mailbox
		SendAt:       when.Unix(),
		Created:      time.Now().Unix(),
		From:         from, // header sender: the owner, or a registered send-as identity
		To:           body.To,
		Cc:           body.Cc,
		Bcc:          body.Bcc,
		Subject:      body.Subject,
		Text:         plain,
		HTML:         body.HTML,
		InReplyTo:    body.InReplyTo,
		Attachments:  atts,
		SMTPHost:     tr.Server,
		SMTPPort:     tr.Port,
		SMTPUseOAuth: tr.UseOAuth,
		OAuthMech:    tr.Mechanism,
		UseSTARTTLS:  tr.UseSTARTTLS,
		InsecureSkip: tr.InsecureSkip,
		Secret:       tr.Secret, // encrypted at rest by Put; never serialized plaintext
	}
	if err := h.schedule.store.Put(rec); err != nil {
		if err == errScheduleQuotaFull {
			return fail(c, fiber.StatusTooManyRequests, "too many pending scheduled sends")
		}
		return fail(c, fiber.StatusInternalServerError, "could not schedule send")
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"scheduled": true,
		"id":        rec.ID,
		"sendAt":    when.UTC().Format(time.RFC3339),
	})
}

// smtpTransport builds the request's SMTP client via the SAME path as an immediate
// send (broker spec or session), then snapshots its connection params + secret so
// a scheduled send can be persisted and re-sent later. Package var override lets
// tests inject a transport without a live SMTP server.
var smtpTransport = func(h *Handler, c *fiber.Ctx) (api.SMTPTransport, error) {
	if spec, ok := brokerSpecOf(c); ok {
		return brokerSMTPClient(spec).Transport(), nil
	}
	cl, err := h.auth.CreateSMTPClient(c)
	if err != nil {
		return api.SMTPTransport{}, err
	}
	return cl.Transport(), nil
}

// smtpTransport is the Handler method wrapper around the seam var.
func (h *Handler) smtpTransport(c *fiber.Ctx) (api.SMTPTransport, error) {
	return smtpTransport(h, c)
}

// --- HTTP handlers ----------------------------------------------------------

// handleListScheduled lists the authenticated account's pending scheduled sends.
// GET /v1/scheduled
func (h *Handler) handleListScheduled(c *fiber.Ctx) error {
	if h.schedule == nil {
		return fail(c, fiber.StatusNotImplemented, "scheduled send is not enabled")
	}
	account := h.fromEmail(c)
	if account == "" {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	recs, err := h.schedule.store.List(account)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not list scheduled sends")
	}
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, scheduleRedactPublic(r))
	}
	return c.JSON(fiber.Map{"scheduled": out})
}

// handleCancelScheduled cancels a pending scheduled send owned by the account.
// A missing id — or one owned by another account — is 404 (no cross-account leak).
// DELETE /v1/scheduled/:id
func (h *Handler) handleCancelScheduled(c *fiber.Ctx) error {
	if h.schedule == nil {
		return fail(c, fiber.StatusNotImplemented, "scheduled send is not enabled")
	}
	account := h.fromEmail(c)
	if account == "" {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id := c.Params("id")

	// Ownership check: Get is keyed by (account, id), so another account's id (or a
	// nonexistent one) misses → 404, revealing nothing about whether it exists for
	// some OTHER account.
	if _, err := h.schedule.store.Get(account, id); err != nil {
		return fail(c, fiber.StatusNotFound, "scheduled send not found")
	}
	if err := h.schedule.store.Delete(account, id); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not cancel scheduled send")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// scheduledPatchBody is the (all-optional) edit payload. Only the provided fields
// change; sendAt is re-validated against the horizon when present.
type scheduledPatchBody struct {
	SendAt  *string `json:"sendAt"`
	Subject *string `json:"subject"`
	Text    *string `json:"text"`
	HTML    *string `json:"html"`
	To      *string `json:"to"`
	Cc      *string `json:"cc"`
	Bcc     *string `json:"bcc"`
}

// handlePatchScheduled edits a pending scheduled send's time and/or body. Only the
// owning account can edit its own record (404 otherwise, no leak).
// PATCH /v1/scheduled/:id
func (h *Handler) handlePatchScheduled(c *fiber.Ctx) error {
	if h.schedule == nil {
		return fail(c, fiber.StatusNotImplemented, "scheduled send is not enabled")
	}
	account := h.fromEmail(c)
	if account == "" {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id := c.Params("id")

	rec, err := h.schedule.store.Get(account, id)
	if err != nil {
		return fail(c, fiber.StatusNotFound, "scheduled send not found")
	}

	var body scheduledPatchBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if body.SendAt != nil {
		when, verr := parseFutureSendAt(*body.SendAt)
		if verr != nil {
			return fail(c, fiber.StatusBadRequest, verr.Error())
		}
		rec.SendAt = when.Unix()
	}
	if body.Subject != nil {
		rec.Subject = *body.Subject
	}
	if body.Text != nil {
		rec.Text = *body.Text
	}
	if body.HTML != nil {
		rec.HTML = *body.HTML
	}
	if body.To != nil {
		rec.To = *body.To
	}
	if body.Cc != nil {
		rec.Cc = *body.Cc
	}
	if body.Bcc != nil {
		rec.Bcc = *body.Bcc
	}

	// Re-encrypt is unnecessary (transport unchanged); Put keeps EncSecret as-is
	// because rec.Secret is empty here (we never decrypted it on this path).
	if err := h.schedule.store.Put(rec); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not update scheduled send")
	}
	return c.JSON(scheduleRedactPublic(rec))
}
