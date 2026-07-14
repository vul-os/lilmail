package jsonapi

// identities_test.go — the send-as identities WRITE path (/v1/settings/identities
// PUT) and the compose From gate.
//
//   - PUT round-trips through the KV; GET still leads with the primary
//   - PUT is auth-gated (no session, no broker → 401) and per-owner isolated
//   - a vulos-mail-hosted mailbox PUSHES the aliases to the engine, which is the
//     AUTHORITY: a refusal (403 "not an address this account may send as") or an
//     unreachable engine stores NOTHING (fail-closed)
//   - malformed / injecting addresses are refused locally (400)
//   - compose sends AS the chosen identity, and an UNREGISTERED From is refused (403)
//
// Send-as only: none of this makes an alias a deliverable inbound address.

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/storage"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// newIdentitiesApp wires a KV-backed brokered /v1 app with a fake IMAP client, so
// the identities surface AND the compose path can be exercised together.
func newIdentitiesApp(t *testing.T) (*fiber.App, *Handler) {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return &fakeMailClient{}, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	kv, err := storage.OpenBolt(t.TempDir() + "/identities.db")
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { kv.Close() })

	store := session.New()
	cfg := &config.Config{}
	cfg.Encryption.Key = parityTestKey
	cfg.Cache.Folder = t.TempDir()
	h := NewWithStore(store, cfg, web.NewAuthHandler(store, cfg), kv)
	t.Cleanup(func() { h.StopScheduler() })

	app := fiber.New()
	h.Register(app)
	return app, h
}

// identDo issues a brokered request. rulesURL != "" marks the mailbox as
// vulos-mail-hosted (the header the identities/vacation/spam pushes derive from).
func identDo(t *testing.T, app *fiber.App, method, target, body, rulesURL string) (int, []byte) {
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
	if rulesURL != "" {
		req.Header.Set(hdrMailRulesURL, rulesURL)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// stubPushIdentities swaps the engine push for a capture (and an optional refusal).
func stubPushIdentities(t *testing.T, captured *[]string, refuse error) {
	t.Helper()
	orig := pushIdentities
	pushIdentities = func(_ context.Context, _, _, _ string, aliases []string) error {
		if refuse != nil {
			return refuse
		}
		*captured = append([]string{}, aliases...)
		return nil
	}
	t.Cleanup(func() { pushIdentities = orig })
}

func identityList(t *testing.T, b []byte) []identity {
	t.Helper()
	var out struct {
		Identities []identity `json:"identities"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode identities: %s", b)
	}
	return out.Identities
}

// PUT stores the aliases; GET reflects them and STILL leads with the primary
// (backward-compatible read contract).
func TestIdentitiesPutRoundTrip(t *testing.T) {
	app, _ := newIdentitiesApp(t)

	code, b := identDo(t, app, "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"Sales@Brand.Example","name":"Sales"},{"address":"user@gmail.com"},{"address":"sales@brand.example"}]}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}
	ids := identityList(t, b)
	// primary first, then the single (de-duplicated, normalized) alias.
	if len(ids) != 2 || ids[0].Address != "user@gmail.com" || !ids[0].IsPrimary {
		t.Fatalf("put echo must lead with the primary: %s", b)
	}
	if ids[1].Address != "sales@brand.example" || ids[1].IsPrimary || ids[1].Name != "Sales" {
		t.Fatalf("alias not stored/normalized: %s", b)
	}

	code, b = identDo(t, app, "GET", "/v1/settings/identities", "", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d %s", code, b)
	}
	ids = identityList(t, b)
	if len(ids) != 2 || !ids[0].IsPrimary || ids[0].Address != "user@gmail.com" || ids[1].Address != "sales@brand.example" {
		t.Fatalf("get: %s", b)
	}

	// PUT replaces the whole set (an empty list removes every alias; the primary stays).
	if code, b = identDo(t, app, "PUT", "/v1/settings/identities", `{"identities":[]}`, ""); code != fiber.StatusOK {
		t.Fatalf("clear: %d %s", code, b)
	}
	if ids = identityList(t, b); len(ids) != 1 || !ids[0].IsPrimary {
		t.Fatalf("cleared set must be just the primary: %s", b)
	}
}

// The write path is authenticated: an unbrokered, unauthenticated PUT is refused
// and stores nothing.
func TestIdentitiesPutAuthGated(t *testing.T) {
	app, _ := newIdentitiesApp(t)

	req := httptest.NewRequest("PUT", "/v1/settings/identities",
		strings.NewReader(`{"identities":[{"address":"sales@brand.example"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT: got %d, want 401", resp.StatusCode)
	}
	// …and nothing was stored for anyone.
	_, b := identDo(t, app, "GET", "/v1/settings/identities", "", "")
	if ids := identityList(t, b); len(ids) != 1 {
		t.Fatalf("unauthenticated PUT must not store: %s", b)
	}
}

// One owner's identities are invisible to another (the KV key is owner-scoped).
func TestIdentitiesIsolation(t *testing.T) {
	app, _ := newParityApp(t)
	doAs(t, app, "alice@corp.com", "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"alice.alias@corp.com"}]}`)
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/settings/identities", "")
	if code != fiber.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if strings.Contains(string(b), "alice.alias@corp.com") {
		t.Fatalf("cross-user identity leak: %s", b)
	}
}

// A vulos-mail-hosted mailbox registers its aliases with the ENGINE, which is the
// authority. The pushed list is exactly the alias addresses (never the primary).
func TestIdentitiesPutPushesAliasesToEngine(t *testing.T) {
	app, _ := newIdentitiesApp(t)
	var pushed []string
	stubPushIdentities(t, &pushed, nil)

	code, b := identDo(t, app, "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"sales@brand.example"},{"address":"user+news@gmail.com"}]}`,
		"http://rules.internal/internal/mailrules")
	if code != fiber.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}
	if len(pushed) != 2 || pushed[0] != "sales@brand.example" || pushed[1] != "user+news@gmail.com" {
		t.Fatalf("aliases pushed to the engine = %v", pushed)
	}
	var out struct {
		ServerEnforced bool `json:"serverEnforced"`
	}
	json.Unmarshal(b, &out)
	if !out.ServerEnforced {
		t.Fatalf("a vulos-mail-hosted save must report serverEnforced: %s", b)
	}
}

// THE security regression: the engine refuses an alias the account may not claim
// (a foreign domain). The refusal is propagated AND nothing is stored — the UI can
// never offer a From the mail server will reject.
func TestIdentitiesPutFailsClosedWhenEngineRefuses(t *testing.T) {
	app, _ := newIdentitiesApp(t)
	var pushed []string
	stubPushIdentities(t, &pushed, &fiber.Error{
		Code: fiber.StatusForbidden, Message: "alias is not an address this account may send as: ceo@google.com",
	})

	code, b := identDo(t, app, "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"ceo@google.com"}]}`, "http://rules.internal/internal/mailrules")
	if code != fiber.StatusForbidden {
		t.Fatalf("engine refusal: got %d (%s), want 403", code, b)
	}
	if !strings.Contains(string(b), "ceo@google.com") {
		t.Fatalf("the engine's reason must be surfaced: %s", b)
	}
	// Nothing stored: GET is back to the primary alone.
	_, b = identDo(t, app, "GET", "/v1/settings/identities", "", "")
	if ids := identityList(t, b); len(ids) != 1 || !ids[0].IsPrimary {
		t.Fatalf("a refused alias must not be stored: %s", b)
	}
	// …and it is not sendable.
	code, b = identDo(t, app, "POST", "/v1/messages",
		`{"to":"bob@x.com","subject":"hi","text":"hi","from":"ceo@google.com"}`, "")
	if code != fiber.StatusForbidden {
		t.Fatalf("send as a refused alias: got %d (%s), want 403", code, b)
	}
}

// An unreachable engine is also fail-closed: 502, and nothing is stored.
func TestIdentitiesPutFailsClosedWhenEngineUnreachable(t *testing.T) {
	app, _ := newIdentitiesApp(t)
	var pushed []string
	stubPushIdentities(t, &pushed, context.DeadlineExceeded)

	code, b := identDo(t, app, "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"sales@brand.example"}]}`, "http://rules.internal/internal/mailrules")
	if code != fiber.StatusBadGateway {
		t.Fatalf("unreachable engine: got %d (%s), want 502", code, b)
	}
	_, b = identDo(t, app, "GET", "/v1/settings/identities", "", "")
	if ids := identityList(t, b); len(ids) != 1 {
		t.Fatalf("an unregistered alias must not be stored: %s", b)
	}
}

// Malformed / header-injecting addresses are refused locally, before anything is
// pushed or stored.
func TestIdentitiesPutRejectsMalformed(t *testing.T) {
	app, _ := newIdentitiesApp(t)
	for _, bad := range []string{
		`{"identities":[{"address":"not-an-address"}]}`,
		`{"identities":[{"address":"a@b@c.com"}]}`,
		`{"identities":[{"address":"@brand.example"}]}`,
		`{"identities":[{"address":"sales@brand.example\r\nBcc: victim@evil.com"}]}`,
		`{"identities":[{"address":"sales@brand.example","name":"Sales\r\nBcc: victim@evil.com"}]}`,
	} {
		if code, b := identDo(t, app, "PUT", "/v1/settings/identities", bad, ""); code != fiber.StatusBadRequest {
			t.Fatalf("%s: got %d (%s), want 400", bad, code, b)
		}
	}
}

// The compose From gate: a REGISTERED identity is what actually goes out on the
// wire; an unregistered address is refused (403) and nothing is sent.
func TestSendUsesChosenIdentityAndRefusesUnregistered(t *testing.T) {
	app, _ := newIdentitiesApp(t)

	cap := &captureSMTP{}
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	// Register an alias (no engine wired here → stored as the client's read model).
	if code, b := identDo(t, app, "PUT", "/v1/settings/identities",
		`{"identities":[{"address":"sales@brand.example"}]}`, ""); code != fiber.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}

	// Send as the registered alias → the assembled MIME carries it as From.
	code, b := identDo(t, app, "POST", "/v1/messages",
		`{"to":"bob@x.com","subject":"hi","text":"hello","from":"sales@brand.example"}`, "")
	if code != fiber.StatusCreated {
		t.Fatalf("send: %d %s", code, b)
	}
	from := fromHeaderOf(t, cap.raw)
	if !strings.Contains(from, "sales@brand.example") || strings.Contains(from, "user@gmail.com") {
		t.Fatalf("the chosen identity must be the From that is sent, got %q:\n%s", from, cap.raw)
	}

	// An UNREGISTERED From is refused, and nothing new is sent.
	cap.raw = nil
	code, b = identDo(t, app, "POST", "/v1/messages",
		`{"to":"bob@x.com","subject":"hi","text":"hello","from":"ceo@google.com"}`, "")
	if code != fiber.StatusForbidden {
		t.Fatalf("unregistered From: got %d (%s), want 403", code, b)
	}
	if cap.raw != nil {
		t.Fatalf("a refused send must not reach SMTP:\n%s", cap.raw)
	}

	// No From at all → the primary mailbox (unchanged behaviour).
	if code, b = identDo(t, app, "POST", "/v1/messages",
		`{"to":"bob@x.com","subject":"hi","text":"hello"}`, ""); code != fiber.StatusCreated {
		t.Fatalf("send without from: %d %s", code, b)
	}
	if from := fromHeaderOf(t, cap.raw); !strings.Contains(from, "user@gmail.com") {
		t.Fatalf("an omitted From must default to the primary mailbox, got %q", from)
	}
}

// fromHeaderOf returns the value of the assembled message's From header (the MIME
// builder renders it as `Name <addr>`), so the test asserts on the ADDRESS that
// actually goes out rather than on an exact rendering.
func fromHeaderOf(t *testing.T, raw []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.ToLower(line), "from:") {
			return strings.TrimSpace(line[len("from:"):])
		}
	}
	t.Fatalf("no From header in the assembled message:\n%s", raw)
	return ""
}
