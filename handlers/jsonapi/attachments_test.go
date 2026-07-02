package jsonapi

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// attachMailClient is a fakeMailClient that returns a fixed attachment so we can
// assert the brokered /v1 attachment path streams content + headers correctly.
type attachMailClient struct {
	*fakeMailClient
	content     []byte
	filename    string
	contentType string
	err         error
	gotFolder   string
	gotUID      string
	gotPart     string
}

func (a *attachMailClient) FetchAttachment(folder, uid, part string) ([]byte, string, string, error) {
	a.gotFolder, a.gotUID, a.gotPart = folder, uid, part
	if a.err != nil {
		return nil, "", "", a.err
	}
	return a.content, a.filename, a.contentType, nil
}

// A brokered request must be able to download an attachment built from the
// X-Vulos-Mail-* headers (no session), with the right Content-Type and
// Content-Disposition, and the folder/uid/part threaded through to the client.
func TestBrokeredAttachmentDownload(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &attachMailClient{
		fakeMailClient: &fakeMailClient{},
		content:        []byte("PDF-BYTES"),
		filename:       "report.pdf",
		contentType:    "application/pdf",
	}
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return fake, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/messages/42/attachments/2.1?folder=INBOX", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type = %q; want application/pdf", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="report.pdf"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "PDF-BYTES" {
		t.Fatalf("body = %q; want PDF-BYTES", body)
	}
	if fake.gotFolder != "INBOX" || fake.gotUID != "42" || fake.gotPart != "2.1" {
		t.Fatalf("folder/uid/part not threaded: %q/%q/%q", fake.gotFolder, fake.gotUID, fake.gotPart)
	}
	if !fake.closed {
		t.Fatalf("brokered client was not Close()d")
	}
}

// A message MIME filename / content type is untrusted; a malicious one carrying
// CR/LF (an HTTP header-injection attempt) must never split the response headers.
// The download must still succeed (200) with a single, sanitized
// Content-Disposition and a safe Content-Type.
func TestAttachmentHeaderInjectionSafe(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &attachMailClient{
		fakeMailClient: &fakeMailClient{},
		content:        []byte("DATA"),
		// Attempt to inject a second header + break out of the filename value.
		filename:    "evil\r\nSet-Cookie: pwned=1\".pdf",
		contentType: "application/pdf\r\nX-Injected: 1",
	}
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return fake, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/messages/42/attachments/2.1?folder=INBOX", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	// The forged header must NOT have materialized.
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("header injection succeeded: Set-Cookie leaked")
	}
	if resp.Header.Get("X-Injected") != "" {
		t.Fatalf("header injection succeeded: X-Injected leaked")
	}
	cd := resp.Header.Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") {
		t.Fatalf("Content-Disposition contains CR/LF: %q", cd)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.ContainsAny(ct, "\r\n") {
		t.Fatalf("Content-Type contains CR/LF: %q", ct)
	}
	// A malformed (injection-bearing) content type falls back to octet-stream.
	if ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q; want application/octet-stream fallback", ct)
	}
}

// An unknown message/part (the IMAP client returns an error) must surface as 404.
func TestAttachmentUnknownPart404(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &attachMailClient{
		fakeMailClient: &fakeMailClient{},
		err:            errors.New("attachment not found"),
	}
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return fake, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/messages/999/attachments/9.9?folder=INBOX", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// Without a valid broker secret and without a session, the attachment route must
// return 401 (and never dial).
func TestAttachmentRequiresAuth(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	called := false
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) {
		called = true
		return &fakeMailClient{}, nil
	}
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/messages/42/attachments/2.1", nil)
	req.Header.Set(hdrBrokerAuth, "WRONG")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite wrong broker secret")
	}
}
