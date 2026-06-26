package jsonapi

import (
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

// recordingMailClient is a MailClient that records the args of MoveMessage so a
// test can assert how the handler resolved the source/destination folders.
type recordingMailClient struct {
	fakeMailClient
	movedSrc  string
	movedUID  string
	movedDest string
}

func (r *recordingMailClient) MoveMessage(src, uid, dest string) error {
	r.movedSrc, r.movedUID, r.movedDest = src, uid, dest
	return nil
}

// newBrokeredApp wires a Handler whose client() resolves to the supplied
// MailClient via the stubbed broker dial seam, and returns a request builder
// that carries valid brokered headers.
func newBrokeredApp(t *testing.T, cl api.MailClient) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)
	return app
}

// A valid brokered POST .../move with a toFolder must return 204 and call
// MoveMessage with the source from ?folder= and the destination from the body.
func TestMoveHappyPath(t *testing.T) {
	rec := &recordingMailClient{}
	app := newBrokeredApp(t, rec)

	req := httptest.NewRequest("POST", "/v1/messages/42/move?folder=INBOX",
		strings.NewReader(`{"toFolder":"Archive"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, b)
	}
	if rec.movedSrc != "INBOX" || rec.movedUID != "42" || rec.movedDest != "Archive" {
		t.Fatalf("move args wrong: src=%q uid=%q dest=%q", rec.movedSrc, rec.movedUID, rec.movedDest)
	}
}

// A non-empty body folder field overrides the ?folder= query param.
func TestMoveBodyFolderOverridesQuery(t *testing.T) {
	rec := &recordingMailClient{}
	app := newBrokeredApp(t, rec)

	req := httptest.NewRequest("POST", "/v1/messages/7/move?folder=INBOX",
		strings.NewReader(`{"folder":"Spam","toFolder":"Archive"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	if _, err := app.Test(req); err != nil {
		t.Fatalf("request: %v", err)
	}
	if rec.movedSrc != "Spam" {
		t.Fatalf("body folder did not override query: src=%q", rec.movedSrc)
	}
}

// A missing/empty toFolder must return 400 JSON.
func TestMoveMissingToFolder(t *testing.T) {
	app := newBrokeredApp(t, &recordingMailClient{})

	req := httptest.NewRequest("POST", "/v1/messages/42/move?folder=INBOX",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, b)
	}
}

// Direct unit coverage of the DemoClient additions.
func TestDemoClientMoveAndTrash(t *testing.T) {
	d := api.NewDemoClient()
	if err := d.MoveMessage("INBOX", "1001", "Archive"); err != nil {
		t.Fatalf("DemoClient.MoveMessage: %v", err)
	}
	trash, err := d.DiscoverTrashFolder()
	if err != nil {
		t.Fatalf("DemoClient.DiscoverTrashFolder: %v", err)
	}
	if trash != "Trash" {
		t.Fatalf("want Trash, got %q", trash)
	}
}
