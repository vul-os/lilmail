package jsonapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/storage"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// captureSchedSMTP records the raw MIME + recipients a scheduled send fires with.
type captureSchedSMTP struct {
	mu    sync.Mutex
	fired bool
	rcpts []string
	raw   []byte
	err   error
}

func (c *captureSchedSMTP) SendRawMessage(rcpts []string, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.fired = true
	c.rcpts, c.raw = rcpts, raw
	return nil
}

func (c *captureSchedSMTP) snapshot() (bool, []string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired, c.rcpts, append([]byte(nil), c.raw...)
}

// schedTestKey is a valid 32-byte AES key for at-rest secret encryption in tests.
const schedTestKey = "0123456789abcdef0123456789abcdef"

// newScheduledApp wires a brokered /v1 app with scheduled send enabled on a temp
// bolt store. It stubs the transport capture (no live SMTP at schedule time) and
// the fire-time SMTP factory (capture instead of dialing), and speeds up polling.
// Returns the app, the store (for direct assertions/restart sims), the fired-send
// capture, and the scheduler.
func newScheduledApp(t *testing.T, cl api.MailClient) (*fiber.App, *scheduleStore, *captureSchedSMTP, *scheduler) {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	// Fast poll so tests don't wait 30s.
	origPoll := schedulePollInterval
	schedulePollInterval = 15 * time.Millisecond
	t.Cleanup(func() { schedulePollInterval = origPoll })

	// Stub IMAP dial.
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	// Stub transport capture: return a plain-auth transport with a known secret.
	origTr := smtpTransport
	smtpTransport = func(_ *Handler, _ *fiber.Ctx) (api.SMTPTransport, error) {
		return api.SMTPTransport{
			Server: "smtp.gmail.com", Port: 587, Email: "user@gmail.com",
			UseSTARTTLS: true, Secret: "the-smtp-password",
		}, nil
	}
	t.Cleanup(func() { smtpTransport = origTr })

	// Capture fires instead of dialing SMTP.
	cap := &captureSchedSMTP{}
	origFactory := scheduleSMTPFactory
	scheduleSMTPFactory = func(rec *scheduledSend) smtpSender {
		// Assert the secret round-tripped through at-rest encryption.
		if rec.Secret != "the-smtp-password" {
			t.Errorf("fire-time secret = %q; want decrypted plaintext", rec.Secret)
		}
		return cap
	}
	t.Cleanup(func() { scheduleSMTPFactory = origFactory })

	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })

	store := session.New()
	cfg := &config.Config{}
	cfg.Encryption.Key = schedTestKey
	h := NewWithStore(store, cfg, web.NewAuthHandler(store, cfg), kv)
	t.Cleanup(func() { h.StopScheduler() })

	app := fiber.New()
	h.Register(app)
	return app, h.schedule.store, cap, h.schedule
}

func doReq(t *testing.T, app *fiber.App, method, target, body string) (*fiber.Ctx, int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return nil, resp.StatusCode, b
}

// waitFired polls until the capture reports a fire, or fails after a timeout.
func waitFired(t *testing.T, cap *captureSchedSMTP) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fired, _, raw := cap.snapshot(); fired {
			return raw
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduled send never fired")
	return nil
}

// A future sendAt persists a scheduled send (202) and does NOT send immediately;
// when its time arrives the drain fires it through the guarded MIME path.
func TestScheduleFiresAndSends(t *testing.T) {
	app, store, cap, _ := newScheduledApp(t, &fakeMailClient{})

	when := time.Now().Add(200 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	// RFC3339 (second precision) is what the API accepts; use a near-future second.
	when = time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)

	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"to":"bob@example.com","subject":"Later","text":"hello","sendAt":"`+when+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", code, b)
	}
	var acc struct {
		Scheduled bool   `json:"scheduled"`
		ID        string `json:"id"`
	}
	json.Unmarshal(b, &acc)
	if !acc.Scheduled || acc.ID == "" {
		t.Fatalf("bad 202 body: %s", b)
	}

	// Not sent yet.
	if fired, _, _ := cap.snapshot(); fired {
		t.Fatal("scheduled send fired immediately")
	}
	// Persisted.
	if recs, _ := store.List("user@gmail.com"); len(recs) != 1 {
		t.Fatalf("want 1 pending record, got %d", len(recs))
	}

	raw := waitFired(t, cap)
	if !strings.Contains(string(raw), "Subject: Later") {
		t.Fatalf("fired MIME missing subject: %s", raw)
	}
	// After a successful send the record is deleted (at-least-once cleanup).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.List("user@gmail.com"); len(recs) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("record not deleted after successful send")
}

// The wave-49 header-injection guard runs at ACTUAL send time: a CRLF-injected
// header in the subject must make the fire fail to build → record dropped, never
// an injected message on the wire.
func TestScheduledSendHeaderGuardAppliesAtSendTime(t *testing.T) {
	app, store, cap, _ := newScheduledApp(t, &fakeMailClient{})

	// Inject via the To header (a structured header the guard validates). A body
	// with CRLF is fine; a To with CRLF must be rejected at build time.
	when := time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)
	inj := `bob@example.com\r\nBcc: evil@example.com`
	payload := `{"to":"` + inj + `","subject":"x","text":"hi","sendAt":"` + when + `"}`

	_, code, b := doReq(t, app, "POST", "/v1/messages", payload)
	if code != fiber.StatusAccepted {
		t.Fatalf("want 202 (schedule accepts; guard runs at fire), got %d: %s", code, b)
	}

	// Wait for the drain to attempt the fire; the build must fail on the guard and
	// drop the record WITHOUT firing an SMTP send.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.List("user@gmail.com"); len(recs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired, _, _ := cap.snapshot(); fired {
		t.Fatal("a header-injected scheduled send reached SMTP — guard did not apply at send time")
	}
	if recs, _ := store.List("user@gmail.com"); len(recs) != 0 {
		t.Fatalf("injected record not dropped: %d remain", len(recs))
	}
}

// A past sendAt is 400; an absurd (far-future) sendAt is 400.
func TestScheduleRejectsPastAndAbsurd(t *testing.T) {
	app, _, _, _ := newScheduledApp(t, &fakeMailClient{})

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, code, _ := doReq(t, app, "POST", "/v1/messages",
		`{"to":"a@b.com","subject":"x","text":"y","sendAt":"`+past+`"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("past sendAt: want 400, got %d", code)
	}

	absurd := time.Now().Add(3 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	_, code, _ = doReq(t, app, "POST", "/v1/messages",
		`{"to":"a@b.com","subject":"x","text":"y","sendAt":"`+absurd+`"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("absurd sendAt: want 400, got %d", code)
	}

	_, code, _ = doReq(t, app, "POST", "/v1/messages",
		`{"to":"a@b.com","subject":"x","text":"y","sendAt":"not-a-time"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("malformed sendAt: want 400, got %d", code)
	}
}

// An OAuth-authed account (short-lived token) is refused a far-future sendAt: the
// token would be expired at fire time, so we reject up front (400) instead of
// accepting a send that cannot succeed. A near-future OAuth send is still allowed.
func TestScheduleOAuthHorizon(t *testing.T) {
	app, _, _, _ := newScheduledApp(t, &fakeMailClient{})

	// Re-stub the transport to report OAuth for this test.
	origTr := smtpTransport
	smtpTransport = func(_ *Handler, _ *fiber.Ctx) (api.SMTPTransport, error) {
		return api.SMTPTransport{
			Server: "smtp.gmail.com", Port: 587, Email: "user@gmail.com",
			UseSTARTTLS: true, UseOAuth: true, Mechanism: "xoauth2", Secret: "ya29.token",
		}, nil
	}
	t.Cleanup(func() { smtpTransport = origTr })

	// Beyond the 12h OAuth horizon → 400.
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"to":"a@b.com","subject":"x","text":"y","sendAt":"`+far+`"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("far OAuth sendAt: want 400, got %d: %s", code, b)
	}

	// Within the horizon → 202.
	near := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	_, code, b = doReq(t, app, "POST", "/v1/messages",
		`{"to":"a@b.com","subject":"x","text":"y","sendAt":"`+near+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("near OAuth sendAt: want 202, got %d: %s", code, b)
	}
}

// GET /v1/scheduled lists only the account's own sends; DELETE cancels before it
// fires. A far-future send is used so it never fires during the test.
func TestScheduleListAndCancel(t *testing.T) {
	app, store, cap, _ := newScheduledApp(t, &fakeMailClient{})

	when := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"to":"c@d.com","subject":"Soon","text":"y","sendAt":"`+when+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("schedule: want 202, got %d: %s", code, b)
	}
	var acc struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &acc)

	_, code, b = doReq(t, app, "GET", "/v1/scheduled", "")
	if code != fiber.StatusOK {
		t.Fatalf("list: want 200, got %d", code)
	}
	var list struct {
		Scheduled []map[string]any `json:"scheduled"`
	}
	json.Unmarshal(b, &list)
	if len(list.Scheduled) != 1 || list.Scheduled[0]["id"] != acc.ID {
		t.Fatalf("list did not return the scheduled send: %s", b)
	}
	if _, hasSecret := list.Scheduled[0]["encSecret"]; hasSecret {
		t.Fatal("list leaked the encrypted secret")
	}

	_, code, _ = doReq(t, app, "DELETE", "/v1/scheduled/"+acc.ID, "")
	if code != fiber.StatusNoContent {
		t.Fatalf("cancel: want 204, got %d", code)
	}
	if recs, _ := store.List("user@gmail.com"); len(recs) != 0 {
		t.Fatalf("record not removed after cancel: %d", len(recs))
	}
	// Give the drain a moment; a cancelled send must never fire.
	time.Sleep(80 * time.Millisecond)
	if fired, _, _ := cap.snapshot(); fired {
		t.Fatal("a cancelled scheduled send still fired")
	}
}

// Per-account isolation: account B cannot list or cancel account A's scheduled
// send. A foreign id is 404 (no cross-account leak) and A's record is untouched.
func TestScheduleAccountIsolation(t *testing.T) {
	app, store, _, _ := newScheduledApp(t, &fakeMailClient{})

	// Seed a record for a DIFFERENT account directly in the store.
	other := &scheduledSend{
		ID: "otherid123", Account: "alice@corp.com", From: "alice@corp.com",
		SendAt: time.Now().Add(time.Hour).Unix(), To: "x@y.com", Subject: "secret",
	}
	if err := store.Put(other); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The brokered request is user@gmail.com. It must not see alice's send.
	_, code, b := doReq(t, app, "GET", "/v1/scheduled", "")
	if code != fiber.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if strings.Contains(string(b), "otherid123") || strings.Contains(string(b), "secret") {
		t.Fatalf("cross-account leak in list: %s", b)
	}

	// Cancelling alice's id as user@gmail.com must 404 and NOT delete it.
	_, code, _ = doReq(t, app, "DELETE", "/v1/scheduled/otherid123", "")
	if code != fiber.StatusNotFound {
		t.Fatalf("foreign cancel: want 404, got %d", code)
	}
	if _, err := store.Get("alice@corp.com", "otherid123"); err != nil {
		t.Fatalf("foreign record was deleted by another account: %v", err)
	}

	// PATCH of a foreign id is also 404.
	_, code, _ = doReq(t, app, "PATCH", "/v1/scheduled/otherid123", `{"subject":"hijacked"}`)
	if code != fiber.StatusNotFound {
		t.Fatalf("foreign patch: want 404, got %d", code)
	}
}

// Restart catch-up: a record that came due while the process was "down" is fired
// promptly on boot (immediate catch-up pass), not only after the first poll tick.
func TestScheduleRestartCatchUp(t *testing.T) {
	// Build a store + seed an already-overdue record, then start a fresh scheduler
	// (simulating a restart that finds an overdue send waiting).
	origFactory := scheduleSMTPFactory
	cap := &captureSchedSMTP{}
	scheduleSMTPFactory = func(*scheduledSend) smtpSender { return cap }
	t.Cleanup(func() { scheduleSMTPFactory = origFactory })

	origPoll := schedulePollInterval
	schedulePollInterval = time.Hour // ensure ONLY the boot catch-up can fire it
	t.Cleanup(func() { schedulePollInterval = origPoll })

	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	store := newScheduleStore(kv, schedTestKey)

	overdue := &scheduledSend{
		ID: "overdue1", Account: "user@gmail.com", From: "user@gmail.com",
		SendAt: time.Now().Add(-1 * time.Minute).Unix(), // already due
		To:     "z@z.com", Subject: "Catchup", Text: "boot",
		SMTPHost: "smtp.gmail.com", SMTPPort: 587, UseSTARTTLS: true,
	}
	if err := store.Put(overdue); err != nil {
		t.Fatalf("seed overdue: %v", err)
	}

	sch := newScheduler(store)
	sch.Start()
	t.Cleanup(sch.Stop)

	// Boot catch-up must fire it despite the 1h poll interval.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fired, _, _ := cap.snapshot(); fired {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("overdue record not fired on boot catch-up")
}

// Quota: a per-account cap bounds pending scheduled sends. Direct store test keeps
// it fast (no need to POST maxPendingPerAccount times over HTTP).
func TestSchedulePerAccountQuota(t *testing.T) {
	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	store := newScheduleStore(kv, schedTestKey)

	for i := 0; i < maxPendingPerAccount; i++ {
		rec := &scheduledSend{
			ID: newScheduleID(), Account: "user@gmail.com", From: "user@gmail.com",
			SendAt: time.Now().Add(time.Hour).Unix(),
		}
		if err := store.Put(rec); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	over := &scheduledSend{
		ID: newScheduleID(), Account: "user@gmail.com", From: "user@gmail.com",
		SendAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := store.Put(over); err != errScheduleQuotaFull {
		t.Fatalf("want quota-full error, got %v", err)
	}
	// A DIFFERENT account is unaffected by the first account's quota.
	other := &scheduledSend{
		ID: newScheduleID(), Account: "other@gmail.com", From: "other@gmail.com",
		SendAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := store.Put(other); err != nil {
		t.Fatalf("second account should not hit first account's quota: %v", err)
	}
}

// The at-rest secret is encrypted: the persisted bytes must not contain the
// plaintext secret, and decryptSecret must recover it.
func TestScheduleSecretEncryptedAtRest(t *testing.T) {
	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	store := newScheduleStore(kv, schedTestKey)

	rec := &scheduledSend{
		ID: "enc1", Account: "user@gmail.com", From: "user@gmail.com",
		SendAt: time.Now().Add(time.Hour).Unix(), Secret: "TOP-SECRET-TOKEN",
	}
	if err := store.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	raw, err := kv.Get(scheduleNS, scheduleKey("user@gmail.com", "enc1"))
	if err != nil {
		t.Fatalf("get raw: %v", err)
	}
	if strings.Contains(string(raw), "TOP-SECRET-TOKEN") {
		t.Fatalf("plaintext secret found in persisted record: %s", raw)
	}
	got, err := store.Get("user@gmail.com", "enc1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Secret != "" {
		t.Fatal("Get must not populate plaintext Secret")
	}
	if err := store.decryptSecret(got); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got.Secret != "TOP-SECRET-TOKEN" {
		t.Fatalf("decrypted secret = %q; want TOP-SECRET-TOKEN", got.Secret)
	}
}
