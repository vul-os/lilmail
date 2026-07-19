package jsonapi

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/config"

	"github.com/gofiber/fiber/v2"
)

// The webmail send path (POST /v1/messages, the route the webmail UI composes
// against) must treat a Bcc address as a REAL delivery — added to the RCPT TO set
// so it is actually sent — while NEVER writing it into the message headers, so no
// To/Cc recipient can see who was blind-copied. A dropped Bcc silently loses mail;
// a leaked Bcc is a privacy breach. This locks both halves against the exact bytes
// handed to SMTP.
func TestWebmailSendDeliversBccWithoutLeaking(t *testing.T) {
	cfg := &config.Config{}
	app := newBrokeredAppCfg(t, cfg, &fakeMailClient{})

	cap := &captureSMTP{}
	orig := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = orig })

	body := `{"to":"bob@to.example","cc":"carol@cc.example","bcc":"dave@secret.example","subject":"quarterly","text":"numbers"}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("send status %d: %s", resp.StatusCode, b)
	}

	// 1. DELIVERED: every address, blind ones included, is in the RCPT TO set.
	for _, want := range []string{"bob@to.example", "carol@cc.example", "dave@secret.example"} {
		if !containsFold(cap.rcpts, want) {
			t.Fatalf("recipient %q missing from RCPT TO set %v (a Bcc must still be delivered)", want, cap.rcpts)
		}
	}

	// 2. NOT LEAKED: the message on the wire carries no blind address and no Bcc header.
	raw := string(cap.raw)
	if strings.Contains(raw, "dave@secret.example") {
		t.Fatalf("PRIVACY BREACH: the Bcc address appears in the sent message:\n%s", raw)
	}
	if strings.Contains(strings.ToLower(raw), "bcc:") {
		t.Fatalf("PRIVACY BREACH: a Bcc header was written to the sent message:\n%s", raw)
	}
	// Sanity: the visible recipients ARE in the headers (so we know we inspected a
	// real message, not an empty capture).
	if !strings.Contains(raw, "bob@to.example") {
		t.Fatalf("expected the To recipient in the message headers:\n%s", raw)
	}
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}
