package jsonapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// mustTime parses an RFC3339 timestamp or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// encPw encrypts a password with the test config key, as the store does at rest.
func encPw(t *testing.T, pw string) string {
	t.Helper()
	enc, err := api.EncryptJSON(pw, parityTestKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return enc
}

// stubDial swaps connectedAccountDial to return cl (or an error) for the test.
func stubDial(t *testing.T, cl api.MailClient, dialErr error) {
	t.Helper()
	orig := connectedAccountDial
	connectedAccountDial = func(string, int, string, string) (api.MailClient, error) {
		return cl, dialErr
	}
	t.Cleanup(func() { connectedAccountDial = orig })
}

func TestAddAndListConnectedAccount(t *testing.T) {
	app, _ := newParityApp(t)
	stubDial(t, &fakeMailClient{}, nil) // add-validation succeeds without a live server

	code, b := doAs(t, app, "user@gmail.com", "POST", "/v1/accounts",
		`{"email":"work@corp.com","password":"pw","label":"Work","color":"#0a0","imapServer":"imap.corp.com","imapPort":993,"smtpServer":"smtp.corp.com","smtpPort":587}`)
	if code != fiber.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	// Response must NOT contain the password (plaintext OR ciphertext field).
	if strings.Contains(strings.ToLower(string(b)), "password") || strings.Contains(string(b), "\"pw\"") {
		t.Fatalf("add response leaked credentials: %s", b)
	}

	code, b = doAs(t, app, "user@gmail.com", "GET", "/v1/accounts", "")
	if code != fiber.StatusOK {
		t.Fatalf("list: %d %s", code, b)
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	json.Unmarshal(b, &out)
	if len(out.Accounts) != 1 || out.Accounts[0]["email"] != "work@corp.com" {
		t.Fatalf("account not listed: %s", b)
	}
	if _, hasPwd := out.Accounts[0]["encryptedPassword"]; hasPwd {
		t.Fatalf("list leaked encryptedPassword: %s", b)
	}
}

func TestAddAccountEncryptedAtRest(t *testing.T) {
	app, h := newParityApp(t)
	stubDial(t, &fakeMailClient{}, nil)

	code, _ := doAs(t, app, "user@gmail.com", "POST", "/v1/accounts",
		`{"email":"work@corp.com","password":"sup3rsecret","imapServer":"imap.corp.com"}`)
	if code != fiber.StatusCreated {
		t.Fatalf("add: %d", code)
	}
	// Inspect the stored record directly: the password must be encrypted, never
	// plaintext, and must decrypt back to the original with the config key.
	st := newAccountsStore(h.kv)
	acct, err := st.get("user@gmail.com", "work@corp.com")
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if acct.EncryptedPassword == "" || strings.Contains(acct.EncryptedPassword, "sup3rsecret") {
		t.Fatalf("password not encrypted at rest: %q", acct.EncryptedPassword)
	}
	var plain string
	if err := api.DecryptJSON(acct.EncryptedPassword, &plain, parityTestKey); err != nil || plain != "sup3rsecret" {
		t.Fatalf("stored password does not round-trip: %v / %q", err, plain)
	}
}

func TestAddAccountRejectsPrimaryShadow(t *testing.T) {
	app, _ := newParityApp(t)
	stubDial(t, &fakeMailClient{}, nil)
	code, _ := doAs(t, app, "user@gmail.com", "POST", "/v1/accounts",
		`{"email":"user@gmail.com","password":"pw","imapServer":"imap.gmail.com"}`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("shadowing primary: want 400, got %d", code)
	}
}

func TestAddAccountBadCredentials(t *testing.T) {
	app, _ := newParityApp(t)
	stubDial(t, nil, errFetchTimeout) // any dial error → 401
	code, _ := doAs(t, app, "user@gmail.com", "POST", "/v1/accounts",
		`{"email":"work@corp.com","password":"wrong","imapServer":"imap.corp.com"}`)
	if code != fiber.StatusUnauthorized {
		t.Fatalf("bad creds: want 401, got %d", code)
	}
}

func TestDeleteConnectedAccountIsolation(t *testing.T) {
	app, h := newParityApp(t)

	// Seed an account owned by alice directly in the store.
	st := newAccountsStore(h.kv)
	if err := st.save("alice@corp.com", connectedAccount{Email: "alice-work@corp.com", IMAPServer: "x"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// user@gmail.com must NOT be able to see or delete alice's account.
	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/accounts", "")
	if code != fiber.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if strings.Contains(string(b), "alice-work@corp.com") {
		t.Fatalf("cross-user account leak in list: %s", b)
	}

	// Deleting alice's account as user@gmail.com is 404 (no-leak) and must NOT
	// remove alice's record.
	code, _ = doAs(t, app, "user@gmail.com", "DELETE", "/v1/accounts/alice-work@corp.com", "")
	if code != fiber.StatusNotFound {
		t.Fatalf("foreign delete: want 404, got %d", code)
	}
	if _, err := st.get("alice@corp.com", "alice-work@corp.com"); err != nil {
		t.Fatalf("foreign delete removed another user's account: %v", err)
	}
}

func TestDeleteOwnAccount(t *testing.T) {
	app, h := newParityApp(t)
	st := newAccountsStore(h.kv)
	st.save("user@gmail.com", connectedAccount{Email: "work@corp.com", IMAPServer: "x"})

	code, _ := doAs(t, app, "user@gmail.com", "DELETE", "/v1/accounts/work@corp.com", "")
	if code != fiber.StatusNoContent {
		t.Fatalf("delete own: want 204, got %d", code)
	}
	if accts, _ := st.list("user@gmail.com"); len(accts) != 0 {
		t.Fatalf("own account not deleted")
	}
}

// TestUnifiedMergesAndTags checks the unified read path merges the primary +
// connected accounts, tags each message, and sorts newest-first.
func TestUnifiedMergesAndTags(t *testing.T) {
	app, h := newParityApp(t)

	// Primary account fetch (broker path) → brokerDialIMAP.
	primary := &tagFakeClient{msgs: []models.Email{
		{ID: "p1", Subject: "primary", Date: mustTime(t, "2026-07-02T10:00:00Z")},
	}}
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return primary, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	// Connected account fetch → connectedAccountDial.
	connected := &tagFakeClient{msgs: []models.Email{
		{ID: "c1", Subject: "connected", Date: mustTime(t, "2026-07-03T10:00:00Z")},
	}}
	stubDial(t, connected, nil)

	// Seed a connected account for the primary owner (with an encrypted password so
	// the unified fetch can decrypt it).
	st := newAccountsStore(h.kv)
	st.save("user@gmail.com", connectedAccount{Email: "work@corp.com", Label: "Work", Color: "#0a0", IMAPServer: "imap.corp.com", EncryptedPassword: encPw(t, "pw")})

	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/unified", "")
	if code != fiber.StatusOK {
		t.Fatalf("unified: %d %s", code, b)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
		Errors   []unifiedError `json:"errors"`
	}
	json.Unmarshal(b, &out)
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 merged messages, got %d: %s", len(out.Messages), b)
	}
	// Newest-first: connected (Jul 3) before primary (Jul 2).
	if out.Messages[0].ID != "c1" || out.Messages[1].ID != "p1" {
		t.Fatalf("not sorted newest-first: %s", b)
	}
	// Each tagged with its source account.
	if out.Messages[0].AccountEmail != "work@corp.com" || out.Messages[0].AccountLabel != "Work" {
		t.Fatalf("connected message not tagged: %+v", out.Messages[0])
	}
	if out.Messages[1].AccountEmail != "user@gmail.com" {
		t.Fatalf("primary message not tagged: %+v", out.Messages[1])
	}
}

// TestUnifiedOneAccountFailsOthersSucceed: a broken connected account reports an
// error but does NOT drop the primary's (or other accounts') messages.
func TestUnifiedOneAccountFailsOthersSucceed(t *testing.T) {
	app, h := newParityApp(t)

	primary := &tagFakeClient{msgs: []models.Email{{ID: "p1", Date: mustTime(t, "2026-07-02T10:00:00Z")}}}
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return primary, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	// Connected account dial fails.
	stubDial(t, nil, errFetchTimeout)

	st := newAccountsStore(h.kv)
	st.save("user@gmail.com", connectedAccount{Email: "broken@corp.com", IMAPServer: "x"})

	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/unified", "")
	if code != fiber.StatusOK {
		t.Fatalf("unified: %d %s", code, b)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
		Errors   []unifiedError `json:"errors"`
	}
	json.Unmarshal(b, &out)
	if len(out.Messages) != 1 || out.Messages[0].ID != "p1" {
		t.Fatalf("primary messages lost when connected failed: %s", b)
	}
	if len(out.Errors) != 1 || out.Errors[0].Account != "broken@corp.com" {
		t.Fatalf("failing account not reported in errors: %s", b)
	}
}

// TestMessagesAccountAllAliasesUnified: GET /v1/messages?account=all routes to the
// unified handler.
func TestMessagesAccountAllAliasesUnified(t *testing.T) {
	app, _ := newParityApp(t)
	primary := &tagFakeClient{msgs: []models.Email{{ID: "p1", Date: mustTime(t, "2026-07-02T10:00:00Z")}}}
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return primary, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	code, b := doAs(t, app, "user@gmail.com", "GET", "/v1/messages?account=all", "")
	if code != fiber.StatusOK {
		t.Fatalf("account=all: %d %s", code, b)
	}
	// The unified shape has an "errors" key; the single-account shape does not.
	if !strings.Contains(string(b), "\"errors\"") {
		t.Fatalf("account=all did not route to unified: %s", b)
	}
}

// tagFakeClient is a fakeMailClient whose FetchMessages returns a fixed list, used
// to exercise the unified merge/tag/sort logic without a live IMAP server.
type tagFakeClient struct {
	fakeMailClient
	msgs []models.Email
}

func (f *tagFakeClient) FetchMessages(string, uint32) ([]models.Email, error) {
	// Return copies so the handler's tagging does not mutate the fixture.
	out := make([]models.Email, len(f.msgs))
	copy(out, f.msgs)
	return out, nil
}
