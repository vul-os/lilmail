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

// --- wave-57 adversarial security regressions -------------------------------

// PATCH-after-schedule injection bypass (vector 2/5): a caller schedules a CLEAN
// send (passes the schedule-time smell test), then PATCHes the To header with a
// CRLF payload to smuggle a Bcc AFTER any schedule-time validation. Because PATCH
// does not itself re-run the header guard, the ONLY thing standing between the
// poisoned record and the wire is the fire-time BuildMIMEMessage guard. This test
// proves that guard fires: the poisoned send must be dropped at drain and never
// reach SMTP. If PATCH ever started persisting a pre-built message that skipped
// BuildMIMEMessage, this would catch the regression.
func TestScheduledPatchInjectionDroppedAtSendTime(t *testing.T) {
	app, store, cap, _ := newScheduledApp(t, &fakeMailClient{})

	// 1) Schedule a clean send, soon but not yet due.
	when := time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)
	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"to":"bob@example.com","subject":"clean","text":"hi","sendAt":"`+when+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("schedule clean: want 202, got %d: %s", code, b)
	}
	var acc struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &acc)

	// 2) PATCH the To with a CRLF-smuggled Bcc — the classic post-validation inject.
	inj := `bob@example.com\r\nBcc: evil@example.com`
	_, code, b = doReq(t, app, "PATCH", "/v1/scheduled/"+acc.ID,
		`{"to":"`+inj+`"}`)
	if code != fiber.StatusOK {
		t.Fatalf("patch: want 200, got %d: %s", code, b)
	}

	// 3) Wait for the drain to try to fire it: the fire-time guard must reject the
	// build and DROP the record, with NO SMTP send.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.List("user@gmail.com"); len(recs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired, _, _ := cap.snapshot(); fired {
		t.Fatal("a PATCH-injected scheduled send reached SMTP — fire-time guard bypassed")
	}
	if recs, _ := store.List("user@gmail.com"); len(recs) != 0 {
		t.Fatalf("PATCH-injected record not dropped at drain: %d remain", len(recs))
	}
}

// PATCH cannot move a send to a PAST or out-of-horizon time (vector 5). The time
// re-validation in handlePatchScheduled must reject the same way the initial
// schedule does, so a caller cannot PATCH a far-future send into the past (which
// would make it fire immediately) or beyond the 1y horizon.
func TestScheduledPatchTimeRevalidated(t *testing.T) {
	app, store, _, _ := newScheduledApp(t, &fakeMailClient{})

	when := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"to":"bob@example.com","subject":"s","text":"hi","sendAt":"`+when+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("schedule: want 202, got %d: %s", code, b)
	}
	var acc struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &acc)

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, code, _ = doReq(t, app, "PATCH", "/v1/scheduled/"+acc.ID, `{"sendAt":"`+past+`"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("PATCH to past time: want 400, got %d", code)
	}

	absurd := time.Now().Add(3 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	_, code, _ = doReq(t, app, "PATCH", "/v1/scheduled/"+acc.ID, `{"sendAt":"`+absurd+`"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("PATCH to out-of-horizon time: want 400, got %d", code)
	}

	// The record's original time must be unchanged after the rejected PATCHes.
	rec, err := store.Get("user@gmail.com", acc.ID)
	if err != nil {
		t.Fatalf("get after rejected patch: %v", err)
	}
	if rec.SendAt <= time.Now().Unix() {
		t.Fatal("rejected PATCH still mutated SendAt into the past")
	}
}

// Crafted-id key crossing (vector 1): an id containing the "|" key separator (or
// path characters) must NOT let a caller address another account's namespace. Even
// if the composed key "<attacker>|<crafted-id>" somehow overlapped another
// account's stored key, Get re-verifies the decoded owner, so a foreign record is
// never returned/cancelled/patched. Proven both via the store directly and via the
// HTTP surface.
func TestScheduledCraftedIDCannotCrossAccount(t *testing.T) {
	app, store, _, _ := newScheduledApp(t, &fakeMailClient{})

	// Seed alice's record whose id is chosen so that a naive prefix join by the
	// attacker could collide: attacker is "user@gmail.com"; craft an id that, when
	// prefixed with "user@gmail.com|", would equal alice's stored key.
	// alice's stored key = "user@gmail.com|X" would require alice.Account to be a
	// suffix game — instead we directly seed alice and try to reach it with a "|" id.
	alice := &scheduledSend{
		ID: "aliceonly", Account: "alice@corp.com", From: "alice@corp.com",
		SendAt: time.Now().Add(time.Hour).Unix(), To: "x@y.com", Subject: "alicesecret",
	}
	if err := store.Put(alice); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Attacker (user@gmail.com) tries an id embedding another account's prefix.
	for _, badID := range []string{
		"alice@corp.com|aliceonly", // try to jump prefix
		"../alice@corp.com|aliceonly",
		"aliceonly\x00",
	} {
		if got, err := store.Get("user@gmail.com", badID); err == nil {
			t.Fatalf("crafted id %q crossed into %q's record", badID, got.Account)
		}
	}

	// Over HTTP: DELETE/PATCH with the crafted id must 404 and leave alice intact.
	_, code, _ := doReq(t, app, "DELETE",
		"/v1/scheduled/"+"alice@corp.com|aliceonly", "")
	if code != fiber.StatusNotFound {
		t.Fatalf("crafted-id DELETE: want 404, got %d", code)
	}
	if _, err := store.Get("alice@corp.com", "aliceonly"); err != nil {
		t.Fatalf("alice's record was affected by crafted-id delete: %v", err)
	}
}

// Fire-time drop is permanent for a permanently-failing BUILD (vector 4): a record
// that can never build (poisoned header) must be dropped, not retried forever. A
// record whose SEND transiently fails, by contrast, is retried (left in place). We
// prove the build-failure drop is terminal by counting that the SMTP factory is
// never invoked for a poisoned record.
func TestScheduledPermanentBuildFailureIsDropped(t *testing.T) {
	origFactory := scheduleSMTPFactory
	var factoryMu sync.Mutex
	var factoryCalls int
	scheduleSMTPFactory = func(*scheduledSend) smtpSender {
		factoryMu.Lock()
		factoryCalls++
		factoryMu.Unlock()
		return &captureSchedSMTP{}
	}
	t.Cleanup(func() { scheduleSMTPFactory = origFactory })

	origPoll := schedulePollInterval
	schedulePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { schedulePollInterval = origPoll })

	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	store := newScheduleStore(kv, schedTestKey)

	poison := &scheduledSend{
		ID: "poison1", Account: "user@gmail.com", From: "user@gmail.com",
		SendAt:  time.Now().Add(-time.Minute).Unix(),
		To:      "bob@example.com\r\nBcc: evil@example.com", // un-buildable
		Subject: "x", Text: "hi",
		SMTPHost: "smtp.gmail.com", SMTPPort: 587, UseSTARTTLS: true,
	}
	if err := store.Put(poison); err != nil {
		t.Fatalf("seed poison: %v", err)
	}

	sch := newScheduler(store)
	sch.Start()
	// Stop blocks until the drain goroutine has fully exited, so the KV can be closed
	// by cleanup without racing a mid-drain access.
	t.Cleanup(sch.Stop)

	// After a few poll cycles the poison record must be gone (dropped on build fail).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.List("user@gmail.com"); len(recs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recs, _ := store.List("user@gmail.com"); len(recs) != 0 {
		t.Fatalf("un-buildable record not dropped: %d remain (retry-forever loop)", len(recs))
	}
	// The build fails BEFORE the SMTP factory is consulted, so it must never be called.
	factoryMu.Lock()
	calls := factoryCalls
	factoryMu.Unlock()
	if calls != 0 {
		t.Fatalf("SMTP factory called %d times for an un-buildable record; build guard should short-circuit", calls)
	}
}

// From is server-forced (vector 1): a client cannot schedule a send that fires AS
// another account. Even if the JSON body carried a "from", the persisted record's
// From/Account are set from the authed identity (fromEmail), not the body.
func TestScheduledFromIsServerForced(t *testing.T) {
	app, store, _, _ := newScheduledApp(t, &fakeMailClient{})

	when := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	// Attempt to smuggle a spoofed "from" in the body.
	_, code, b := doReq(t, app, "POST", "/v1/messages",
		`{"from":"ceo@victim.com","to":"bob@example.com","subject":"s","text":"hi","sendAt":"`+when+`"}`)
	if code != fiber.StatusAccepted {
		t.Fatalf("schedule: want 202, got %d: %s", code, b)
	}
	// The record must be owned by and fire AS the authed account, not the spoof.
	recs, err := store.List("user@gmail.com")
	if err != nil || len(recs) != 1 {
		t.Fatalf("want 1 record for authed account, got %d (err %v)", len(recs), err)
	}
	if recs[0].From != "user@gmail.com" || recs[0].Account != "user@gmail.com" {
		t.Fatalf("From/Account not server-forced: From=%q Account=%q", recs[0].From, recs[0].Account)
	}
	// The spoofed account must have nothing.
	if r2, _ := store.List("ceo@victim.com"); len(r2) != 0 {
		t.Fatalf("spoofed from created a record under victim account: %d", len(r2))
	}
}

// alwaysFailSMTP fails every SendRawMessage and counts how many times it was tried.
type alwaysFailSMTP struct {
	mu    sync.Mutex
	tries int
}

func (a *alwaysFailSMTP) SendRawMessage([]string, []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tries++
	return io.ErrClosedPipe // stand-in for a permanent SMTP failure (e.g. auth 535)
}

func (a *alwaysFailSMTP) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.tries }

// Permanently-failing SEND is abandoned, not retried forever (vector 4). A record
// whose SMTP send never succeeds must, after a bounded number of attempts, be
// dropped — otherwise it re-dials SMTP every poll, pins the encrypted credential
// in storage, and permanently burns a per-account quota slot. This proves the
// retry budget (maxSendAttempts) terminates the loop.
func TestScheduledPermanentSendFailureAbandonedAfterBudget(t *testing.T) {
	origMax := maxSendAttempts
	maxSendAttempts = 3 // keep the test fast
	t.Cleanup(func() { maxSendAttempts = origMax })

	fail := &alwaysFailSMTP{}
	origFactory := scheduleSMTPFactory
	scheduleSMTPFactory = func(*scheduledSend) smtpSender { return fail }
	t.Cleanup(func() { scheduleSMTPFactory = origFactory })

	origPoll := schedulePollInterval
	schedulePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { schedulePollInterval = origPoll })

	kv, err := storage.OpenBolt(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	store := newScheduleStore(kv, schedTestKey)

	rec := &scheduledSend{
		ID: "failforever", Account: "user@gmail.com", From: "user@gmail.com",
		SendAt:  time.Now().Add(-time.Minute).Unix(),
		To:      "bob@example.com", Subject: "s", Text: "hi",
		SMTPHost: "smtp.gmail.com", SMTPPort: 587, UseSTARTTLS: true,
		Secret:  "the-smtp-password", // so it round-trips through re-persist on retry
	}
	if err := store.Put(rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sch := newScheduler(store)
	sch.Start()
	t.Cleanup(sch.Stop) // Stop blocks until the drain exits; safe to close KV after.

	// The record must eventually be abandoned (deleted) rather than looping forever.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.List("user@gmail.com"); len(recs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recs, _ := store.List("user@gmail.com"); len(recs) != 0 {
		t.Fatalf("permanently-failing send not abandoned: %d remain (infinite retry loop)", len(recs))
	}
	// It must have been tried at least the budget, and — crucially — the loop must
	// have STOPPED: give it more polls and confirm no further attempts.
	stopped := fail.count()
	if stopped < maxSendAttempts {
		t.Fatalf("abandoned too early: %d tries < budget %d", stopped, maxSendAttempts)
	}
	time.Sleep(150 * time.Millisecond) // ~15 more poll cycles
	if after := fail.count(); after != stopped {
		t.Fatalf("send retried after abandonment: %d → %d (loop did not terminate)", stopped, after)
	}
}
