package jsonapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// recordingFolders records folder-management + move calls so tests can assert on them.
type recordingFolders struct {
	fakeMailClient
	created    string
	deleted    string
	movedSrc   string
	movedDest  string
	movedUID   string
	fetchMsgID string
}

func (r *recordingFolders) CreateMailbox(name string) error { r.created = name; return nil }
func (r *recordingFolders) DeleteMailbox(name string) error { r.deleted = name; return nil }
func (r *recordingFolders) MoveMessage(src, uid, dest string) error {
	r.movedSrc, r.movedUID, r.movedDest = src, uid, dest
	return nil
}
func (r *recordingFolders) FetchSingleMessage(string, string) (models.Email, error) {
	return models.Email{MessageID: r.fetchMsgID}, nil
}

// fReq builds a brokered JSON request with valid broker headers.
func fReq(method, target, body string) *http.Request {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	extra := map[string]string{"Content-Type": "application/json"}
	for k, v := range brokeredHeaders() {
		extra[k] = v
	}
	return brokeredReq(method, target, rdr, extra)
}

func TestCreateFolderHappyPath(t *testing.T) {
	rec := &recordingFolders{}
	app := newBrokeredApp(t, rec)

	resp, err := app.Test(fReq("POST", "/v1/folders", `{"name":"Projects"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, b)
	}
	if rec.created != "Projects" {
		t.Fatalf("created=%q, want Projects", rec.created)
	}
}

func TestCreateFolderRejectsSystemName(t *testing.T) {
	rec := &recordingFolders{}
	app := newBrokeredApp(t, rec)
	resp, err := app.Test(fReq("POST", "/v1/folders", `{"name":"Trash"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	if rec.created != "" {
		t.Fatalf("must not create a system folder, got %q", rec.created)
	}
}

func TestDeleteFolderProtectsSystemFolders(t *testing.T) {
	for _, name := range []string{"INBOX", "Sent", "Spam", "Trash", "inbox", "Foo/Trash"} {
		rec := &recordingFolders{}
		app := newBrokeredApp(t, rec)
		resp, err := app.Test(fReq("DELETE", "/v1/folders", `{"name":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusForbidden {
			t.Fatalf("delete %q: want 403, got %d", name, resp.StatusCode)
		}
		if rec.deleted != "" {
			t.Fatalf("delete %q: must not delete system folder", name)
		}
	}
}

func TestDeleteFolderUserFolder(t *testing.T) {
	rec := &recordingFolders{}
	app := newBrokeredApp(t, rec)
	resp, err := app.Test(fReq("DELETE", "/v1/folders", `{"name":"Projects"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, b)
	}
	if rec.deleted != "Projects" {
		t.Fatalf("deleted=%q, want Projects", rec.deleted)
	}
}

func TestReportSpamMovesToJunk(t *testing.T) {
	rec := &recordingFolders{}
	app := newBrokeredApp(t, rec)
	resp, err := app.Test(fReq("POST", "/v1/messages/99/spam?folder=INBOX", ""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	if rec.movedSrc != "INBOX" || rec.movedUID != "99" || rec.movedDest != "Spam" {
		t.Fatalf("move args: src=%q uid=%q dest=%q", rec.movedSrc, rec.movedUID, rec.movedDest)
	}
}

// Snooze moves the message to Snoozed and registers a due-time when a rule-store
// URL is brokered (so the snooze endpoint can be derived).
func TestSnoozeMovesAndSchedules(t *testing.T) {
	rec := &recordingFolders{fetchMsgID: "<abc@x>"}
	app := newBrokeredApp(t, rec)

	var gotURL, gotAccount, gotMsgID, gotMethod string
	orig := postSnoozeSchedule
	postSnoozeSchedule = func(_ context.Context, storeURL, _, method, account, messageID string, _ time.Time) error {
		gotURL, gotAccount, gotMsgID, gotMethod = storeURL, account, messageID, method
		return nil
	}
	t.Cleanup(func() { postSnoozeSchedule = orig })

	req := fReq("POST", "/v1/messages/5/snooze?folder=INBOX", `{"until":"2030-01-01T10:00:00Z"}`)
	req.Header.Set(hdrMailRulesURL, "http://127.0.0.1:2080/internal/mailrules")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, b)
	}
	if rec.movedDest != "Snoozed" || rec.movedSrc != "INBOX" {
		t.Fatalf("move: src=%q dest=%q", rec.movedSrc, rec.movedDest)
	}
	if gotURL != "http://127.0.0.1:2080/internal/snooze" {
		t.Fatalf("snooze store URL=%q", gotURL)
	}
	if gotAccount != "user@gmail.com" || gotMsgID != "<abc@x>" || gotMethod != http.MethodPost {
		t.Fatalf("schedule args: account=%q msgID=%q method=%q", gotAccount, gotMsgID, gotMethod)
	}
}

// Without a brokered rule-store URL, snooze still moves the message but reports
// autoReturn:false (honest degrade) with a 200 body.
func TestSnoozeWithoutRuleStoreDegrades(t *testing.T) {
	rec := &recordingFolders{fetchMsgID: "<abc@x>"}
	app := newBrokeredApp(t, rec)

	resp, err := app.Test(fReq("POST", "/v1/messages/5/snooze?folder=INBOX", `{"until":"2030-01-01T10:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"autoReturn":false`) {
		t.Fatalf("want autoReturn:false, got %s", body)
	}
	if rec.movedDest != "Snoozed" {
		t.Fatalf("message should still be moved to Snoozed, dest=%q", rec.movedDest)
	}
}

func TestSnoozeStoreURLDerivation(t *testing.T) {
	cases := map[string]string{
		"http://h:2080/internal/mailrules":  "http://h:2080/internal/snooze",
		"http://h:2080/internal/mailrules/": "http://h:2080/internal/snooze",
		"":                                  "",
	}
	for in, want := range cases {
		if got := snoozeStoreURLFromRules(in); got != want {
			t.Fatalf("snoozeStoreURLFromRules(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestDemoClientFolderOps(t *testing.T) {
	d := api.NewDemoClient()
	if err := d.CreateMailbox("X"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteMailbox("X"); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.DiscoverSnoozedFolder(); s != "Snoozed" {
		t.Fatalf("snoozed=%q", s)
	}
	if j, _ := d.DiscoverJunkFolder(); j != "Spam" {
		t.Fatalf("junk=%q", j)
	}
}
