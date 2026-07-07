package jsonapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// fakeMailClient is a no-network MailClient used to assert that the brokered
// path builds a client from headers without contacting a live IMAP server.
type fakeMailClient struct {
	folders []*api.MailboxInfo
	closed  bool
}

func (f *fakeMailClient) FetchFolders() ([]*api.MailboxInfo, error) { return f.folders, nil }
func (f *fakeMailClient) FetchMessages(string, uint32) ([]models.Email, error) {
	return nil, nil
}
func (f *fakeMailClient) FetchMessagesPaged(string, uint32, uint32) ([]models.Email, error) {
	return nil, nil
}
func (f *fakeMailClient) FetchSingleMessage(string, string) (models.Email, error) {
	return models.Email{}, nil
}
func (f *fakeMailClient) SearchMessages(string, string, uint32) ([]models.Email, error) {
	return nil, nil
}
func (f *fakeMailClient) FetchAttachment(string, string, string) ([]byte, string, string, error) {
	return nil, "", "", nil
}
func (f *fakeMailClient) DeleteMessage(string, string) error                { return nil }
func (f *fakeMailClient) SetMessageFlag(string, string, string, bool) error { return nil }
func (f *fakeMailClient) MoveMessage(string, string, string) error          { return nil }
func (f *fakeMailClient) SaveToSent(string, string, string, []byte) error   { return nil }
func (f *fakeMailClient) SaveDraft([]byte) error                            { return nil }
func (f *fakeMailClient) DeleteDraft(string) error                          { return nil }
func (f *fakeMailClient) DeleteMessageFromFolder(string, string) error      { return nil }
func (f *fakeMailClient) DiscoverDraftsFolder() (string, error)             { return "Drafts", nil }
func (f *fakeMailClient) DiscoverTrashFolder() (string, error)              { return "Trash", nil }
func (f *fakeMailClient) DiscoverSnoozedFolder() (string, error)            { return "Snoozed", nil }
func (f *fakeMailClient) DiscoverJunkFolder() (string, error)               { return "Spam", nil }
func (f *fakeMailClient) CreateMailbox(string) error                        { return nil }
func (f *fakeMailClient) DeleteMailbox(string) error                        { return nil }
func (f *fakeMailClient) WatchInbox(<-chan struct{}, func(models.Email)) error {
	return nil
}
func (f *fakeMailClient) Close() error { f.closed = true; return nil }

// withStubbedDial swaps brokerDialIMAP for the duration of a test and records
// the spec it was called with.
func withStubbedDial(t *testing.T, cl *fakeMailClient, captured *brokerSpec) {
	t.Helper()
	orig := brokerDialIMAP
	brokerDialIMAP = func(spec brokerSpec) (api.MailClient, error) {
		if captured != nil {
			*captured = spec
		}
		return cl, nil
	}
	t.Cleanup(func() { brokerDialIMAP = orig })
}

func brokeredHeaders() map[string]string {
	return map[string]string{
		hdrMailProvider: "gmail",
		hdrMailEmail:    "user@gmail.com",
		hdrMailUsername: "user@gmail.com",
		hdrMailAuth:     "xoauth2",
		hdrMailSecret:   "ya29.access-token",
		hdrMailIMAPHost: "imap.gmail.com",
		hdrMailIMAPPort: "993",
		hdrMailSMTPHost: "smtp.gmail.com",
		hdrMailSMTPPort: "587",
	}
}

// A request carrying a VALID broker secret + mailbox headers must build the
// MailClient directly from those headers (via the dial seam), bypassing session
// auth entirely.
func TestBrokeredRequestWithValidSecretBuildsClientFromHeaders(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &fakeMailClient{folders: []*api.MailboxInfo{{Name: "INBOX"}}}
	var got brokerSpec
	withStubbedDial(t, fake, &got)

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/folders", nil)
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

	// The spec must have been parsed from the headers verbatim.
	if got.Email != "user@gmail.com" || got.Auth != "xoauth2" ||
		got.Secret != "ya29.access-token" || got.IMAPHost != "imap.gmail.com" || got.IMAPPort != 993 {
		t.Fatalf("spec not parsed from headers: %+v", got)
	}
	if !fake.closed {
		t.Fatalf("brokered client was not Close()d")
	}

	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Folders []*api.MailboxInfo `json:"folders"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if len(out.Folders) != 1 || out.Folders[0].Name != "INBOX" {
		t.Fatalf("folders not served from brokered client: %s", body)
	}
}

// plain auth must dial via NewClient (password), not the OAuth path.
func TestBrokeredPlainAuthSpec(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var got brokerSpec
	withStubbedDial(t, &fakeMailClient{}, &got)

	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/folders", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	req.Header.Set(hdrMailEmail, "me@example.org")
	req.Header.Set(hdrMailAuth, "plain")
	req.Header.Set(hdrMailSecret, "hunter2")
	req.Header.Set(hdrMailIMAPHost, "imap.example.org")
	// No username header -> defaults to email; no SMTP host -> defaults to IMAP host.

	if _, err := app.Test(req); err != nil {
		t.Fatalf("request: %v", err)
	}
	if got.Auth != "plain" || got.Username != "me@example.org" || got.IMAPPort != 993 || got.SMTPHost != "imap.example.org" {
		t.Fatalf("plain spec defaults wrong: %+v", got)
	}
}

// A request with the WRONG broker secret must NOT trust the headers: the dial
// seam must not be invoked, and with no session it falls back to 401.
func TestBrokeredRequestWithWrongSecretIgnoresHeaders(t *testing.T) {
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

	req := httptest.NewRequest("GET", "/v1/folders", nil)
	req.Header.Set(hdrBrokerAuth, "WRONG")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 (headers ignored, no session), got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite wrong broker secret — credential trust leak")
	}
}

// With LILMAIL_BROKER_SECRET UNSET, the whole brokered path is disabled: headers
// are ignored even if a (would-be) broker auth header is present.
func TestBrokeredPathDisabledWhenSecretUnset(t *testing.T) {
	t.Setenv(brokerEnvSecret, "")

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

	req := httptest.NewRequest("GET", "/v1/folders", nil)
	req.Header.Set(hdrBrokerAuth, "anything")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 (brokered path disabled), got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked while LILMAIL_BROKER_SECRET unset")
	}
}

// A valid broker secret but an UNKNOWN auth mechanism must fail closed: an
// attacker who somehow learns the shared secret still cannot smuggle a client in
// via an unsupported SASL mech name — parseBroker rejects anything that is not
// xoauth2/plain, so the dial seam is never reached and we fall back to 401.
func TestBrokeredRequestWithUnknownAuthMechIgnored(t *testing.T) {
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

	req := httptest.NewRequest("GET", "/v1/folders", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailAuth, "digest-md5") // unsupported mechanism

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 (unknown auth mech ignored), got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked for an unknown broker auth mechanism")
	}
}

// Valid secret but missing essential mailbox headers must fall back (ignored).
func TestBrokeredRequestWithIncompleteHeadersIgnored(t *testing.T) {
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

	req := httptest.NewRequest("GET", "/v1/folders", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	req.Header.Set(hdrMailEmail, "user@gmail.com") // secret + imap host missing

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 (incomplete headers ignored), got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite incomplete brokered headers")
	}
}
