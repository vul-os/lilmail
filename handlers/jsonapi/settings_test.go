package jsonapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/web"
	"lilmail/storage"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

const parityTestKey = "0123456789abcdef0123456789abcdef" // 32 bytes for AES-256

// newParityApp wires a KV-backed brokered /v1 app for the parity surfaces
// (settings + accounts). Returns the app and the live Handler so tests can reach
// the stores directly for seeding/assertions.
func newParityApp(t *testing.T) (*fiber.App, *Handler) {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	kv, err := storage.OpenBolt(t.TempDir() + "/parity.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })

	store := session.New()
	cfg := &config.Config{}
	cfg.Encryption.Key = parityTestKey
	h := NewWithStore(store, cfg, web.NewAuthHandler(store, cfg), kv)
	t.Cleanup(func() { h.StopScheduler() })

	app := fiber.New()
	h.Register(app)
	return app, h
}

// doAs issues a brokered request as a specific mailbox identity (email), so tests
// can simulate two different users hitting the same server. All headers besides
// the identity come from brokeredHeaders().
func doAs(t *testing.T, app *fiber.App, email, method, target, body string) (int, []byte) {
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
	req.Header.Set(hdrMailEmail, email)
	req.Header.Set(hdrMailUsername, email)
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// --- Vacation ----------------------------------------------------------------

func TestVacationRoundTrip(t *testing.T) {
	app, _ := newParityApp(t)

	// Default (never set) → enabled:false.
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/vacation", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d %s", code, b)
	}
	var got vacationConfig
	json.Unmarshal(b, &got)
	if got.Enabled {
		t.Fatalf("unset vacation should be disabled: %s", b)
	}

	// PUT a config.
	code, b = doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/vacation",
		`{"enabled":true,"subject":"OOO","body":"<b>Away</b> until Monday","respondOnlyToContacts":true}`)
	if code != fiber.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}
	// GET reflects it.
	code, b = doAs(t, app, "user@gmail.com", "GET", "/v1/settings/vacation", "")
	json.Unmarshal(b, &got)
	if !got.Enabled || got.Subject != "OOO" || !got.RespondOnlyToContacts {
		t.Fatalf("vacation not persisted: %s", b)
	}
}

func TestVacationSubjectHeaderInjectionRejected(t *testing.T) {
	app, _ := newParityApp(t)
	// A CRLF in the subject would forge a header on the auto-reply — must 400.
	code, _ := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/vacation",
		"{\"enabled\":true,\"subject\":\"OOO\\r\\nBcc: victim@evil.com\",\"body\":\"x\"}")
	if code != fiber.StatusBadRequest {
		t.Fatalf("header-injection subject: want 400, got %d", code)
	}
}

func TestVacationBodySanitized(t *testing.T) {
	app, _ := newParityApp(t)
	code, _ := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/vacation",
		`{"enabled":true,"subject":"OOO","body":"hi<script>alert(1)</script><a href=\"javascript:evil()\">x</a>"}`)
	if code != fiber.StatusOK {
		t.Fatalf("put: %d", code)
	}
	_, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/vacation", "")
	if strings.Contains(strings.ToLower(string(b)), "<script") || strings.Contains(strings.ToLower(string(b)), "javascript:") {
		t.Fatalf("vacation body not sanitized: %s", b)
	}
}

func TestVacationBadDates(t *testing.T) {
	app, _ := newParityApp(t)
	// endAt before startAt.
	code, _ := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/vacation",
		`{"enabled":true,"subject":"OOO","body":"x","startAt":"2026-08-01T00:00:00Z","endAt":"2026-07-01T00:00:00Z"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("reversed window: want 400, got %d", code)
	}
	// Non-RFC3339.
	code, _ = doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/vacation",
		`{"enabled":true,"subject":"OOO","body":"x","startAt":"next tuesday"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("bad date: want 400, got %d", code)
	}
}

func TestVacationIsolation(t *testing.T) {
	app, _ := newParityApp(t)
	// user A sets a vacation with a secret in the subject.
	doAs(t, app, "alice@corp.com", "PUT", "/v1/settings/vacation",
		`{"enabled":true,"subject":"alice-secret","body":"x"}`)
	// user B must NOT see it (own default only).
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/vacation", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if strings.Contains(string(b), "alice-secret") {
		t.Fatalf("cross-user vacation leak: %s", b)
	}
	var got vacationConfig
	json.Unmarshal(b, &got)
	if got.Enabled {
		t.Fatalf("B saw A's enabled state: %s", b)
	}
}

// --- Signatures --------------------------------------------------------------

func TestSignaturesRoundTripAndSanitize(t *testing.T) {
	app, _ := newParityApp(t)
	code, b := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/signatures",
		`{"signatures":[{"name":"Work","html":"<b>Jane</b><script>x</script>","default":true}]}`)
	if code != fiber.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}
	var out struct {
		Signatures []signature `json:"signatures"`
	}
	json.Unmarshal(b, &out)
	if len(out.Signatures) != 1 || out.Signatures[0].ID == "" {
		t.Fatalf("no server-assigned id: %s", b)
	}
	if strings.Contains(strings.ToLower(out.Signatures[0].HTML), "<script") {
		t.Fatalf("signature not sanitized: %s", out.Signatures[0].HTML)
	}
	// GET returns it.
	code, b = doAs(t, app, "user@gmail.com", "GET", "/v1/settings/signatures", "")
	if code != fiber.StatusOK || !strings.Contains(string(b), "Work") {
		t.Fatalf("get signatures: %d %s", code, b)
	}
}

func TestSignaturesRejectTwoDefaults(t *testing.T) {
	app, _ := newParityApp(t)
	code, _ := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/signatures",
		`{"signatures":[{"name":"A","html":"a","default":true},{"name":"B","html":"b","default":true}]}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("two defaults: want 400, got %d", code)
	}
}

func TestSignaturesRejectNoName(t *testing.T) {
	app, _ := newParityApp(t)
	code, _ := doAs(t, app, "user@gmail.com", "PUT", "/v1/settings/signatures",
		`{"signatures":[{"html":"a"}]}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("no name: want 400, got %d", code)
	}
}

func TestSignaturesIsolation(t *testing.T) {
	app, _ := newParityApp(t)
	doAs(t, app, "alice@corp.com", "PUT", "/v1/settings/signatures",
		`{"signatures":[{"name":"alice-sig","html":"secret"}]}`)
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/signatures", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if strings.Contains(string(b), "alice-sig") || strings.Contains(string(b), "secret") {
		t.Fatalf("cross-user signature leak: %s", b)
	}
}

// --- Identities --------------------------------------------------------------

func TestIdentitiesAlwaysIncludePrimary(t *testing.T) {
	app, _ := newParityApp(t)
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/identities", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d %s", code, b)
	}
	var out struct {
		Identities []identity `json:"identities"`
	}
	json.Unmarshal(b, &out)
	if len(out.Identities) < 1 || out.Identities[0].Address != "user@gmail.com" || !out.Identities[0].IsPrimary {
		t.Fatalf("primary identity missing/incorrect: %s", b)
	}
}

// --- 501 when no KV wired ----------------------------------------------------

func TestSettings501WithoutStore(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg)) // no KV
	app := fiber.New()
	h.Register(app)

	code, _ := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/vacation", "")
	if code != fiber.StatusNotImplemented {
		t.Fatalf("no-KV vacation: want 501, got %d", code)
	}
	code, _ = doAs(t, app, "user@gmail.com", "GET", "/v1/accounts", "")
	if code != fiber.StatusNotImplemented {
		t.Fatalf("no-KV accounts: want 501, got %d", code)
	}
}
