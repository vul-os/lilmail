package jsonapi

import (
	"io"
	"net/http/httptest"
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
	gotFolder   string
	gotUID      string
	gotPart     string
}

func (a *attachMailClient) FetchAttachment(folder, uid, part string) ([]byte, string, string, error) {
	a.gotFolder, a.gotUID, a.gotPart = folder, uid, part
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
