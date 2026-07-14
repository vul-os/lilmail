package jsonapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// capturedSpam records the (account, cfg) the handler would have brokered to
// vulos-mail, so tests assert validation + owner threading without a live server.
type capturedSpam struct {
	getAccount string
	putAccount string
	putCfg     spamConfig
}

func spamApp(t *testing.T, cap *capturedSpam, stored spamConfig, upErr error) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	origGet, origPut := brokerSpamGet, brokerSpamPut
	brokerSpamGet = func(_ context.Context, _, _, account string) (spamConfig, error) {
		cap.getAccount = account
		if upErr != nil {
			return spamConfig{}, upErr
		}
		return stored, nil
	}
	brokerSpamPut = func(_ context.Context, _, _, account string, cfg spamConfig) (spamConfig, error) {
		cap.putAccount = account
		cap.putCfg = cfg
		if upErr != nil {
			return spamConfig{}, upErr
		}
		return cfg, nil
	}
	t.Cleanup(func() { brokerSpamGet, brokerSpamPut = origGet, origPut })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

// spamDo issues a brokered request to /v1/settings/spam.
func spamDo(t *testing.T, app *fiber.App, method, body string, withRulesURL bool) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/v1/settings/spam", r)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	if withRulesURL {
		req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestSpamSettings_GetBrokersToOwner(t *testing.T) {
	cap := &capturedSpam{}
	stored := spamConfig{Sensitivity: "high", DNSBL: true, Allow: []string{"a@x.example"}, Block: []string{"bad.example"}}
	app := spamApp(t, cap, stored, nil)

	code, body := spamDo(t, app, "GET", "", true)
	if code != 200 {
		t.Fatalf("GET: %d (%s)", code, body)
	}
	// The brokered account is the authenticated owner (broker Email), NOT client input.
	if cap.getAccount != "user@gmail.com" {
		t.Fatalf("owner not threaded to broker: %q", cap.getAccount)
	}
	var out spamConfig
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Sensitivity != "high" || !out.DNSBL || len(out.Allow) != 1 {
		t.Fatalf("GET body mismatch: %+v", out)
	}
}

func TestSpamSettings_PutRoundTripAndOwner(t *testing.T) {
	cap := &capturedSpam{}
	app := spamApp(t, cap, spamConfig{}, nil)

	body := `{"sensitivity":"HIGH","dnsbl":true,"allow":["Friend@Corp.Example","trusted.example"],"block":["spammer@bad.example"]}`
	code, resp := spamDo(t, app, "PUT", body, true)
	if code != 200 {
		t.Fatalf("PUT: %d (%s)", code, resp)
	}
	if cap.putAccount != "user@gmail.com" {
		t.Fatalf("PUT owner not threaded: %q", cap.putAccount)
	}
	// Handler normalized (lowercased + defaulted) before brokering.
	if cap.putCfg.Sensitivity != "high" {
		t.Fatalf("sensitivity not normalized: %q", cap.putCfg.Sensitivity)
	}
	if len(cap.putCfg.Allow) != 2 || cap.putCfg.Allow[0] != "friend@corp.example" {
		t.Fatalf("allow not normalized: %v", cap.putCfg.Allow)
	}
}

func TestSpamSettings_Validation(t *testing.T) {
	cap := &capturedSpam{}
	app := spamApp(t, cap, spamConfig{}, nil)
	bad := []string{
		`{"sensitivity":"extreme"}`,                // not in the enum
		`{"block":["evil@x.example\r\nBcc: v@x"]}`, // CRLF header injection
		`{"allow":["a@x.example\u0000"]}`,          // embedded NUL
		`{"block":["a b@x.example"]}`,              // internal whitespace
		`{"allow":["localhost"]}`,                  // bare single label (no dot)
		`{"block":["a@@x.example"]}`,               // double @
		`{"allow":["notés@x.example"]}`,            // non-ASCII
	}
	for _, b := range bad {
		code, resp := spamDo(t, app, "PUT", b, true)
		if code != 400 {
			t.Fatalf("expected 400 for %q, got %d (%s)", b, code, resp)
		}
	}
	// A rejected PUT must never reach the broker (fail-safe).
	if cap.putAccount != "" {
		t.Fatalf("invalid PUT leaked to broker: %q", cap.putAccount)
	}
}

func TestSpamSettings_HonestDegradeNoRulesURL(t *testing.T) {
	cap := &capturedSpam{}
	app := spamApp(t, cap, spamConfig{}, nil)
	// No X-Vulos-Mail-Rules-Url → not a vulos-mail-hosted mailbox → 501.
	if code, _ := spamDo(t, app, "GET", "", false); code != fiber.StatusNotImplemented {
		t.Fatalf("GET no-rules-url: want 501, got %d", code)
	}
	if code, _ := spamDo(t, app, "PUT", `{"sensitivity":"low"}`, false); code != fiber.StatusNotImplemented {
		t.Fatalf("PUT no-rules-url: want 501, got %d", code)
	}
	if cap.getAccount != "" || cap.putAccount != "" {
		t.Fatalf("degraded request should never broker upstream")
	}
}

func TestSpamSettings_UpstreamStatusPropagates(t *testing.T) {
	cap := &capturedSpam{}
	// Upstream returns a 501 (settings storage not configured on vulos-mail).
	app := spamApp(t, cap, spamConfig{}, &spamAPIError{status: fiber.StatusNotImplemented, msg: "settings storage not configured"})
	if code, _ := spamDo(t, app, "GET", "", true); code != fiber.StatusNotImplemented {
		t.Fatalf("upstream 501 should propagate, got %d", code)
	}
}
