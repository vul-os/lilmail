// handlers/jsonapi/accounts.go — /v1/accounts CRUD + unified-inbox read path.
//
// PORTED FROM handlers/web/{accounts,unified,accountstore}.go (the legacy HTMX
// multi-account feature) to the /v1 JSON surface. What carried over unchanged in
// spirit: concurrent per-account IMAP fetch (one goroutine + timeout per account,
// one slow/broken account never blocks the others), per-account AES-GCM-encrypted
// credentials at rest, and per-account tags (label/color) merged into one list.
// What changed: storage moved onto the storage.KV seam (accounts_store.go) keyed
// by the authenticated owner (fromEmail) so isolation is structural, and the
// surface is JSON, not HTML fragments. The legacy "switch active session" flow is
// intentionally NOT ported — a rich client reads all accounts at once (unified),
// it does not swap the session identity.
//
// ISOLATION: owner == fromEmail on every call. A user lists/adds/removes/reads
// only their own accounts; another user's account is 404 (no-leak). Credentials
// are never returned in cleartext (the response shape omits the password field).
package jsonapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// Fixed, opaque per-account fetch errors. They are surfaced to the client in the
// unified `errors` array; keeping them fixed avoids leaking server/credential
// detail (e.g. a raw IMAP auth-failure string) back to the caller.
var (
	errDecryptCreds = errors.New("account credentials could not be read")
	errFetchTimeout = errors.New("account timed out")
)

// connectedAccountDial builds an IMAP client for a connected account by
// server/port/username/password. Package var so the add-account validation and
// the unified fetch can be tested without a live IMAP server; defaults to the
// same api.NewClient the rest of lilmail uses (plain-auth; connected accounts are
// password mailboxes, not OAuth — OAuth accounts arrive via the broker path).
var connectedAccountDial = func(server string, port int, username, password string) (api.MailClient, error) {
	return api.NewClient(server, port, username, password)
}

// sanitizeFetchErr maps a fetch error to a stable, non-leaky client message. Known
// sentinels pass their (already-safe) text; anything else collapses to a generic
// string so an upstream IMAP error can't echo server internals to the client.
func sanitizeFetchErr(err error) string {
	switch {
	case errors.Is(err, errDecryptCreds):
		return errDecryptCreds.Error()
	case errors.Is(err, errFetchTimeout):
		return errFetchTimeout.Error()
	default:
		return "account fetch failed"
	}
}

// unifiedAccountTimeout bounds one account's fetch in the unified read path, so a
// single slow/unreachable account cannot stall the merged response. Ported from
// the legacy unifiedFetchTimeout.
const unifiedAccountTimeout = 10 * time.Second

// unifiedHardCap bounds the merged message count regardless of account count, so a
// unified fetch cannot return an unbounded list.
const unifiedHardCap = 200

// registerAccounts mounts /v1/accounts and the unified read path.
func (h *Handler) registerAccounts(g fiber.Router) {
	g.Get("/accounts", h.handleListConnectedAccounts)
	g.Post("/accounts", h.handleAddConnectedAccount)
	g.Delete("/accounts/:email", h.handleDeleteConnectedAccount)
	// Unified inbox: merge the primary + every connected account. Also reachable as
	// GET /v1/messages?account=all (handled in messages.go) so the client can use a
	// single listing endpoint; this explicit path is the canonical form.
	g.Get("/unified", h.handleUnified)
}

// accountsStoreOr501 resolves the accounts store + authenticated owner. When it
// returns handled==true it has ALREADY written the error response (501 no KV, 401
// no identity) and the caller must return herr immediately.
func (h *Handler) accountsStoreOr501(c *fiber.Ctx) (store *accountsStore, owner string, handled bool, herr error) {
	if h.kv == nil {
		return nil, "", true, fail(c, fiber.StatusNotImplemented, "connected accounts are not enabled")
	}
	owner = h.fromEmail(c)
	if strings.TrimSpace(owner) == "" {
		return nil, "", true, fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	return newAccountsStore(h.kv), owner, false, nil
}

// connectedAccountPublic is the client-safe view — NEVER includes the encrypted
// (let alone plaintext) password.
func connectedAccountPublic(a connectedAccount) fiber.Map {
	return fiber.Map{
		"email":      a.Email,
		"label":      a.Label,
		"color":      a.Color,
		"imapServer": a.IMAPServer,
		"imapPort":   a.IMAPPort,
		"smtpServer": a.SMTPServer,
		"smtpPort":   a.SMTPPort,
	}
}

// handleListConnectedAccounts returns the caller's connected accounts (no secrets).
func (h *Handler) handleListConnectedAccounts(c *fiber.Ctx) error {
	store, owner, handled, herr := h.accountsStoreOr501(c)
	if handled {
		return herr
	}
	accts, err := store.list(owner)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not list accounts")
	}
	out := make([]fiber.Map, 0, len(accts))
	for _, a := range accts {
		out = append(out, connectedAccountPublic(a))
	}
	return c.JSON(fiber.Map{"accounts": out})
}

// handleAddConnectedAccount validates the new account's IMAP credentials by
// opening (and immediately closing) a live connection, then stores the account
// with its password AES-GCM encrypted at rest. Ported from web HandleAddAccount.
// POST /v1/accounts  body {email,password,label?,color?,imapServer?,imapPort?,smtpServer?,smtpPort?}
func (h *Handler) handleAddConnectedAccount(c *fiber.Ctx) error {
	store, owner, handled, herr := h.accountsStoreOr501(c)
	if handled {
		return herr
	}
	var req struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		Label      string `json:"label"`
		Color      string `json:"color"`
		IMAPServer string `json:"imapServer"`
		IMAPPort   int    `json:"imapPort"`
		SMTPServer string `json:"smtpServer"`
		SMTPPort   int    `json:"smtpPort"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	req.Email = strings.TrimSpace(req.Email)
	req.Password = strings.TrimSpace(req.Password)
	if req.Email == "" || req.Password == "" {
		return fail(c, fiber.StatusBadRequest, "email and password are required")
	}
	// A connected account must not shadow the primary mailbox.
	if strings.EqualFold(req.Email, owner) {
		return fail(c, fiber.StatusBadRequest, "that is already your primary account")
	}

	// Fill IMAP/SMTP coordinates from global config when the client omits them.
	if req.IMAPServer == "" {
		req.IMAPServer = h.config.IMAP.Server
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = h.config.IMAP.Port
	}
	if req.SMTPServer == "" {
		req.SMTPServer = h.config.SMTP.Server
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = h.config.SMTP.GetPort()
	}
	if req.Label == "" {
		req.Label = req.Email
	}
	if req.IMAPServer == "" {
		return fail(c, fiber.StatusBadRequest, "imapServer is required")
	}

	username := req.Email
	if !h.config.Server.UsernameIsEmail {
		username = api.GetUsernameFromEmail(req.Email)
	}
	if username == "" {
		return fail(c, fiber.StatusBadRequest, "invalid email format")
	}

	// Validate the credentials against the live IMAP server (open + close).
	client, err := connectedAccountDial(req.IMAPServer, req.IMAPPort, username, req.Password)
	if err != nil {
		return fail(c, fiber.StatusUnauthorized, "IMAP login failed")
	}
	client.Close()

	// Encrypt the password at rest with the application key — never store plaintext.
	encPwd, err := api.EncryptJSON(req.Password, h.config.Encryption.Key)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not secure credentials")
	}

	acct := connectedAccount{
		Email:             req.Email,
		Label:             req.Label,
		Color:             req.Color,
		IMAPServer:        req.IMAPServer,
		IMAPPort:          req.IMAPPort,
		SMTPServer:        req.SMTPServer,
		SMTPPort:          req.SMTPPort,
		EncryptedPassword: encPwd,
	}
	if err := store.save(owner, acct); err != nil {
		if err == errAccountsQuotaFull {
			return fail(c, fiber.StatusTooManyRequests, "too many connected accounts")
		}
		return fail(c, fiber.StatusInternalServerError, "could not save account")
	}
	return c.Status(fiber.StatusCreated).JSON(connectedAccountPublic(acct))
}

// handleDeleteConnectedAccount removes one of the caller's connected accounts. A
// missing account — or one belonging to another user — is 404 (no cross-user
// leak): the ownership check is a Get keyed by (owner, email).
// DELETE /v1/accounts/:email
func (h *Handler) handleDeleteConnectedAccount(c *fiber.Ctx) error {
	store, owner, handled, herr := h.accountsStoreOr501(c)
	if handled {
		return herr
	}
	email := strings.TrimSpace(c.Params("email"))
	if email == "" {
		return fail(c, fiber.StatusBadRequest, "email param required")
	}
	if _, err := store.get(owner, email); err != nil {
		return fail(c, fiber.StatusNotFound, "account not found")
	}
	if err := store.delete(owner, email); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not remove account")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleUnified merges the primary account + every connected account into one
// newest-first list, each message tagged with its source account (email/label/
// color). Ported from web FetchUnified: concurrent per-account fetch, per-account
// timeout, one failing account never breaks the others (its error is reported in
// the `errors` array alongside the messages that did load).
// GET /v1/unified  ?folder=&limit=
func (h *Handler) handleUnified(c *fiber.Ctx) error {
	store, owner, handled, herr := h.accountsStoreOr501(c)
	if handled {
		return herr
	}
	folder := folderParam(c)
	// Clamp the per-account fetch size. The merged response is capped at
	// unifiedHardCap regardless, so asking any single account's IMAP server for
	// more than that is pointless AND lets a caller push a hostile limit (e.g.
	// ?limit=4000000000) straight into every connected account's FetchMessages —
	// a memory/time amplification. Bound it to the hard cap here.
	limit := uintQuery(c, "limit", 50)
	if limit == 0 || limit > unifiedHardCap {
		limit = unifiedHardCap
	}

	accts, err := store.list(owner)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not list accounts")
	}

	merged, fetchErrs := h.fetchUnified(c, owner, folder, limit, accts)
	return c.JSON(fiber.Map{
		"folder":   folder,
		"messages": merged,
		"errors":   fetchErrs, // per-account failures (empty when all succeeded)
	})
}

// unifiedError is the client-facing per-account failure entry.
type unifiedError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

// fetchUnified runs the primary fetch (via the request's own client — session or
// broker) plus one goroutine per connected account, merges the results newest-
// first, tags each message with its source, and caps the total. Per-account errors
// are collected, never fatal.
func (h *Handler) fetchUnified(c *fiber.Ctx, owner, folder string, limit uint32, accts []connectedAccount) ([]models.Email, []unifiedError) {
	type result struct {
		email string
		label string
		color string
		msgs  []models.Email
		err   error
	}

	results := make([]result, len(accts)+1)

	var wg sync.WaitGroup

	// Primary account: reuse the request's own client construction (session OR
	// brokered). It is fetched in its own goroutine too so a slow primary does not
	// serialize ahead of the connected accounts.
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := result{email: owner, label: owner}
		cl, err := h.client(c)
		if err != nil {
			r.err = err
			results[0] = r
			return
		}
		defer cl.Close()
		msgs, err := cl.FetchMessages(folder, limit)
		r.msgs, r.err = msgs, err
		results[0] = r
	}()

	// Connected accounts: one goroutine each, each with its own timeout + its own
	// fresh IMAP connection (no shared client), decrypting the stored password only
	// transiently in memory.
	for i, a := range accts {
		wg.Add(1)
		go func(idx int, acct connectedAccount) {
			defer wg.Done()
			r := result{email: acct.Email, label: acct.Label, color: acct.Color}
			if r.label == "" {
				r.label = acct.Email
			}
			r.msgs, r.err = h.fetchAccountMessages(acct, folder, limit)
			results[idx+1] = r
		}(i, a)
	}
	wg.Wait()

	var merged []models.Email
	var errs []unifiedError
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, unifiedError{Account: r.email, Error: sanitizeFetchErr(r.err)})
			continue
		}
		for i := range r.msgs {
			r.msgs[i].AccountEmail = r.email
			r.msgs[i].AccountLabel = r.label
			r.msgs[i].AccountColor = r.color
			merged = append(merged, r.msgs[i])
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Date.After(merged[j].Date) })

	cap := int(limit) * len(results)
	if cap > unifiedHardCap || cap <= 0 {
		cap = unifiedHardCap
	}
	if len(merged) > cap {
		merged = merged[:cap]
	}
	if merged == nil {
		merged = []models.Email{}
	}
	if errs == nil {
		errs = []unifiedError{}
	}
	return merged, errs
}

// fetchAccountMessages opens a fresh IMAP connection for one connected account,
// decrypts its password transiently, fetches `folder`, and closes — all bounded by
// unifiedAccountTimeout so a hung account cannot stall the merge.
func (h *Handler) fetchAccountMessages(acct connectedAccount, folder string, limit uint32) ([]models.Email, error) {
	ctx, cancel := context.WithTimeout(context.Background(), unifiedAccountTimeout)
	defer cancel()

	type out struct {
		msgs []models.Email
		err  error
	}
	done := make(chan out, 1)
	go func() {
		var password string
		if err := api.DecryptJSON(acct.EncryptedPassword, &password, h.config.Encryption.Key); err != nil {
			done <- out{nil, errDecryptCreds}
			return
		}
		username := acct.Email
		if !h.config.Server.UsernameIsEmail {
			username = api.GetUsernameFromEmail(acct.Email)
		}
		cl, err := connectedAccountDial(acct.IMAPServer, acct.IMAPPort, username, password)
		if err != nil {
			done <- out{nil, err}
			return
		}
		defer cl.Close()
		msgs, err := cl.FetchMessages(folder, limit)
		done <- out{msgs, err}
	}()

	select {
	case <-ctx.Done():
		return nil, errFetchTimeout
	case r := <-done:
		return r.msgs, r.err
	}
}
