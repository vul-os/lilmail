package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// brokeredReq builds a brokered /v1 request carrying the valid broker secret,
// the standard mailbox headers, and any caller-supplied extra headers. Cuts the
// per-test boilerplate of setting the ~10 broker headers by hand.
func brokeredReq(method, path string, body io.Reader, extra map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return req
}

// --- richClient: a MailClient with programmable returns for the read/delete
// paths that the base fakeMailClient stubs to zero values. ---------------------

type richClient struct {
	*fakeMailClient

	single    models.Email
	singleErr error

	searchReturn []models.Email
	searchErr    error
	searchQuery  string

	// delete-path bookkeeping
	trashFolder   string
	trashErr      error
	moved         [][3]string // {src, uid, dest}
	moveErr       error
	deleted       [][2]string // {folder, uid}
	deleteErr     error
	savedDraft    bool
	saveDraftErr  error
	saveToSentErr error
}

func (r *richClient) FetchSingleMessage(folder, uid string) (models.Email, error) {
	return r.single, r.singleErr
}
func (r *richClient) SearchMessages(folder, q string, limit uint32) ([]models.Email, error) {
	r.searchQuery = q
	return r.searchReturn, r.searchErr
}
func (r *richClient) DiscoverTrashFolder() (string, error) { return r.trashFolder, r.trashErr }
func (r *richClient) MoveMessage(src, uid, dest string) error {
	r.moved = append(r.moved, [3]string{src, uid, dest})
	return r.moveErr
}
func (r *richClient) DeleteMessage(folder, uid string) error {
	r.deleted = append(r.deleted, [2]string{folder, uid})
	return r.deleteErr
}
func (r *richClient) SaveDraft(raw []byte) error { r.savedDraft = true; return r.saveDraftErr }
func (r *richClient) SaveToSent(to, subj, plain string, raw []byte) error {
	return r.saveToSentErr
}

func newRich() *richClient { return &richClient{fakeMailClient: &fakeMailClient{}} }

// --- handleMe ---------------------------------------------------------------

// GET /v1/me in brokered mode must report the brokered mailbox identity (from
// the validated headers), never a session identity.
func TestHandleMe_BrokeredIdentity(t *testing.T) {
	app := newBrokeredAppCfg(t, &config.Config{}, newRich())

	resp, err := app.Test(brokeredReq("GET", "/v1/me", nil, nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Email    string `json:"email"`
		Username string `json:"username"`
	}
	decode(t, resp.Body, &out)
	if out.Email != "user@gmail.com" {
		t.Fatalf("me.email = %q; want brokered mailbox", out.Email)
	}
	if out.Username != "user@gmail.com" {
		t.Fatalf("me.username = %q; want brokered username", out.Username)
	}
}

// --- handleMessage ----------------------------------------------------------

// GET /v1/messages/:uid returns the single message as JSON.
func TestHandleMessage_OK(t *testing.T) {
	rc := newRich()
	rc.single = models.Email{ID: "42", Subject: "Hello"}
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, err := app.Test(brokeredReq("GET", "/v1/messages/42?folder=INBOX", nil, nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out models.Email
	decode(t, resp.Body, &out)
	if out.ID != "42" || out.Subject != "Hello" {
		t.Fatalf("message body = %+v", out)
	}
}

// A message the IMAP client cannot fetch surfaces as 404 (not 500/502).
func TestHandleMessage_NotFound(t *testing.T) {
	rc := newRich()
	rc.singleErr = errors.New("no such message")
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("GET", "/v1/messages/999", nil, nil))
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// --- handleSearch -----------------------------------------------------------

// GET /v1/search must reject a missing q with 400 (before touching the client).
func TestHandleSearch_MissingQuery400(t *testing.T) {
	app := newBrokeredAppCfg(t, &config.Config{}, newRich())

	resp, _ := app.Test(brokeredReq("GET", "/v1/search?folder=INBOX", nil, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for missing q, got %d", resp.StatusCode)
	}
}

// GET /v1/search threads the query through and echoes results.
func TestHandleSearch_OK(t *testing.T) {
	rc := newRich()
	rc.searchReturn = []models.Email{{ID: "5"}, {ID: "6"}}
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, err := app.Test(brokeredReq("GET", "/v1/search?folder=INBOX&q=invoice&limit=10", nil, nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if rc.searchQuery != "invoice" {
		t.Fatalf("query not threaded: %q", rc.searchQuery)
	}
	var out struct {
		Query    string         `json:"query"`
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if out.Query != "invoice" || len(out.Messages) != 2 {
		t.Fatalf("search body = %+v", out)
	}
}

// A backend search error surfaces as 502.
func TestHandleSearch_BackendError(t *testing.T) {
	rc := newRich()
	rc.searchErr = errors.New("imap search failed")
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("GET", "/v1/search?q=x", nil, nil))
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

// --- handleDelete -----------------------------------------------------------

// Default DELETE soft-deletes by MOVING the message to a distinct Trash folder.
func TestHandleDelete_SoftMovesToTrash(t *testing.T) {
	rc := newRich()
	rc.trashFolder = "Trash"
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, err := app.Test(brokeredReq("DELETE", "/v1/messages/42?folder=INBOX", nil, nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(rc.moved) != 1 || rc.moved[0] != [3]string{"INBOX", "42", "Trash"} {
		t.Fatalf("expected soft-delete move to Trash, got %+v", rc.moved)
	}
	if len(rc.deleted) != 0 {
		t.Fatalf("soft delete must not hard-expunge: %+v", rc.deleted)
	}
}

// ?hard=true forces a permanent expunge, bypassing the Trash move entirely.
func TestHandleDelete_HardExpunges(t *testing.T) {
	rc := newRich()
	rc.trashFolder = "Trash" // present, but hard=true must ignore it
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/messages/7?folder=INBOX&hard=true", nil, nil))
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(rc.moved) != 0 {
		t.Fatalf("hard delete must not move to Trash: %+v", rc.moved)
	}
	if len(rc.deleted) != 1 || rc.deleted[0] != [2]string{"INBOX", "7"} {
		t.Fatalf("expected hard expunge, got %+v", rc.deleted)
	}
}

// When the source folder already IS Trash, soft delete falls through to a hard
// expunge (no self-move).
func TestHandleDelete_AlreadyInTrashFallsThroughToHard(t *testing.T) {
	rc := newRich()
	rc.trashFolder = "Trash"
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/messages/7?folder=Trash", nil, nil))
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(rc.moved) != 0 {
		t.Fatalf("must not self-move within Trash: %+v", rc.moved)
	}
	if len(rc.deleted) != 1 {
		t.Fatalf("expected fall-through hard expunge, got %+v", rc.deleted)
	}
}

// When no Trash folder can be discovered, soft delete also falls through to hard.
func TestHandleDelete_NoTrashFallsThroughToHard(t *testing.T) {
	rc := newRich()
	rc.trashErr = errors.New("no trash special-use")
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/messages/7?folder=INBOX", nil, nil))
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(rc.deleted) != 1 {
		t.Fatalf("expected fall-through hard expunge, got %+v", rc.deleted)
	}
}

// A backend delete error surfaces as 502.
func TestHandleDelete_BackendError(t *testing.T) {
	rc := newRich()
	rc.trashErr = errors.New("no trash")
	rc.deleteErr = errors.New("expunge failed")
	app := newBrokeredAppCfg(t, &config.Config{}, rc)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/messages/7?folder=INBOX", nil, nil))
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

// --- handleSaveDraft --------------------------------------------------------

// An entirely empty draft body must be rejected 400 (before building MIME).
func TestHandleSaveDraft_EmptyRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, newRich())

	resp, _ := app.Test(brokeredReq("POST", "/v1/drafts", strings.NewReader(`{}`), nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for empty draft, got %d", resp.StatusCode)
	}
}

// A minimal draft is assembled and appended to the Drafts folder (SaveDraft),
// returning 201 {saved:true}.
func TestHandleSaveDraft_OK(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	rc := newRich()
	app := newBrokeredAppCfg(t, cfg, rc)

	body := `{"to":"a@b.com","subject":"WIP","text":"draft body"}`
	resp, err := app.Test(brokeredReq("POST", "/v1/drafts", strings.NewReader(body), nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, b)
	}
	if !rc.savedDraft {
		t.Fatalf("SaveDraft was not invoked")
	}
	var out struct {
		Saved bool `json:"saved"`
	}
	decode(t, resp.Body, &out)
	if !out.Saved {
		t.Fatalf("want {saved:true}, got %+v", out)
	}
}

// A SaveDraft backend failure surfaces as 502.
func TestHandleSaveDraft_BackendError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	rc := newRich()
	rc.saveDraftErr = errors.New("append failed")
	app := newBrokeredAppCfg(t, cfg, rc)

	resp, _ := app.Test(brokeredReq("POST", "/v1/drafts",
		strings.NewReader(`{"to":"a@b.com","subject":"x","text":"y"}`), nil))
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

// An HTML-only draft (no text) must derive a plain-text fallback via
// stripHTMLForPlain rather than failing.
func TestHandleSaveDraft_HTMLOnlyDerivesPlain(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	rc := newRich()
	app := newBrokeredAppCfg(t, cfg, rc)

	body := `{"to":"a@b.com","subject":"h","html":"<p>Hello <b>world</b></p>"}`
	resp, _ := app.Test(brokeredReq("POST", "/v1/drafts", strings.NewReader(body), nil))
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	if !rc.savedDraft {
		t.Fatalf("SaveDraft not invoked for HTML-only draft")
	}
}

// stripHTMLForPlain unit: tags dropped, whitespace collapsed.
func TestStripHTMLForPlain(t *testing.T) {
	got := stripHTMLForPlain("<p>Hello&nbsp;<b>there</b></p>\n\n<div>  friend </div>")
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("tags leaked: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	if !strings.Contains(got, "there") || !strings.Contains(got, "friend") {
		t.Fatalf("text content lost: %q", got)
	}
}

// --- handleSend validation --------------------------------------------------

// A send missing the required fields (to/subject/body) is rejected 400 before
// any SMTP contact.
func TestHandleSend_MissingFields400(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, newRich())

	cases := []string{
		`{"subject":"x","text":"y"}`,            // no to
		`{"to":"a@b.com","text":"y"}`,           // no subject
		`{"to":"a@b.com","subject":"x"}`,        // no body
		`{"to":"   ","subject":"x","text":"y"}`, // blank to
	}
	for _, body := range cases {
		resp, _ := app.Test(brokeredReq("POST", "/v1/messages", strings.NewReader(body), nil))
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("body %q: want 400, got %d", body, resp.StatusCode)
		}
	}
}

// A malformed JSON body is rejected 400.
func TestHandleSend_MalformedJSON400(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, newRich())

	resp, _ := app.Test(brokeredReq("POST", "/v1/messages", strings.NewReader(`{"to":`), nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for malformed JSON, got %d", resp.StatusCode)
	}
}

// --- calendar CRUD + degradation --------------------------------------------

// PUT /v1/calendar/events/:uid with a valid body updates via the brokered CalDAV
// client and returns {updated:true}. (Exercises handleUpdateEvent success path.)
func TestHandleUpdateEvent_OK(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &fakeCalDAV{}, nil)

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{"summary":"Sync","start":"2026-06-26T10:00:00Z","end":"2026-06-26T11:00:00Z"}`
	resp, _ := app.Test(brokeredReq("PUT", "/v1/calendar/events/evt-9", strings.NewReader(body),
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
}

// PUT with a bad (non-RFC3339) start is rejected 400.
func TestHandleUpdateEvent_BadStart400(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &fakeCalDAV{}, nil)
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{"summary":"x","start":"not-a-date","end":"2026-06-26T11:00:00Z"}`
	resp, _ := app.Test(brokeredReq("PUT", "/v1/calendar/events/evt-9", strings.NewReader(body),
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for bad start, got %d", resp.StatusCode)
	}
}

// PUT with a missing summary is rejected 400.
func TestHandleUpdateEvent_MissingSummary400(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &fakeCalDAV{}, nil)
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{"start":"2026-06-26T10:00:00Z","end":"2026-06-26T11:00:00Z"}`
	resp, _ := app.Test(brokeredReq("PUT", "/v1/calendar/events/evt-9", strings.NewReader(body),
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for missing summary, got %d", resp.StatusCode)
	}
}

// PUT for a brokered account WITHOUT a CalDAV URL must degrade to 501, never
// touching the session.
func TestHandleUpdateEvent_NoCalDAV501(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) { called = true; return &fakeCalDAV{}, nil }
	t.Cleanup(func() { brokerDialCalDAV = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{"summary":"x","start":"2026-06-26T10:00:00Z","end":"2026-06-26T11:00:00Z"}`
	resp, _ := app.Test(brokeredReq("PUT", "/v1/calendar/events/evt-9", strings.NewReader(body), nil))
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite missing CalDAV URL")
	}
}

// DELETE /v1/calendar/events/:uid removes via the brokered client (204).
func TestHandleDeleteEvent_OK(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &fakeCalDAV{}, nil)
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/calendar/events/evt-1", nil,
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
}

// DELETE where the CalDAV backend reports failure surfaces as 404 (event not found).
func TestHandleDeleteEvent_BackendError404(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &errCalDAV{err: errors.New("gone")}, nil)
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/calendar/events/evt-1", nil,
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// DELETE for a brokered account WITHOUT a CalDAV URL degrades to 501.
func TestHandleDeleteEvent_NoCalDAV501(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) { called = true; return &fakeCalDAV{}, nil }
	t.Cleanup(func() { brokerDialCalDAV = orig })
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/calendar/events/evt-1", nil, nil))
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite missing CalDAV URL")
	}
}

// GET /v1/calendar/freebusy returns busy slots from the brokered client.
func TestHandleFreeBusy_OK(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	withStubbedCalDAVDial(t, &fakeCalDAV{busy: []models.FreeBusySlot{{}}}, nil)
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("GET", "/v1/calendar/freebusy", nil,
		map[string]string{hdrMailCalDAVURL: "https://dav.example.com/caldav/"}))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Busy []models.FreeBusySlot `json:"busy"`
	}
	decode(t, resp.Body, &out)
	if len(out.Busy) != 1 {
		t.Fatalf("busy slots = %+v", out.Busy)
	}
}

// freebusy for a brokered account WITHOUT a CalDAV URL degrades to an empty
// {busy:[]} (never a 5xx, never a session touch).
func TestHandleFreeBusy_NoCalDAVEmpty(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) { called = true; return &fakeCalDAV{}, nil }
	t.Cleanup(func() { brokerDialCalDAV = orig })
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("GET", "/v1/calendar/freebusy", nil, nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite missing CalDAV URL")
	}
	var out struct {
		Busy []models.FreeBusySlot `json:"busy"`
	}
	decode(t, resp.Body, &out)
	if out.Busy == nil || len(out.Busy) != 0 {
		t.Fatalf("want empty busy array")
	}
}

// errCalDAV is a calDAVClient whose write ops fail, to exercise backend-error paths.
type errCalDAV struct{ err error }

func (e *errCalDAV) ListEvents(context.Context, time.Time, time.Time) ([]models.CalendarEvent, error) {
	return nil, e.err
}
func (e *errCalDAV) CreateEvent(context.Context, models.CalendarEvent) error { return e.err }
func (e *errCalDAV) UpdateEvent(context.Context, models.CalendarEvent) error { return e.err }
func (e *errCalDAV) DeleteEvent(context.Context, string) error               { return e.err }
func (e *errCalDAV) FreeBusy(context.Context, time.Time, time.Time) ([]models.FreeBusySlot, error) {
	return nil, e.err
}

// --- contacts CRUD update/delete + degradation ------------------------------

// PUT /v1/contacts/:uid routes through the bearer PUT seam and returns the saved
// card, forwarding the path :uid as the contact UID.
func TestHandleUpdateContact_OK(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	var got models.Contact
	orig := brokerContactPut
	brokerContactPut = func(spec brokerSpec, ct models.Contact) (models.Contact, error) {
		got = ct
		ct.Path = "/ab/u9.vcf"
		return ct, nil
	}
	t.Cleanup(func() { brokerContactPut = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{"name":"Carol","emails":["carol@x.com"]}`
	resp, _ := app.Test(brokeredReq("PUT", "/v1/contacts/u9", strings.NewReader(body),
		map[string]string{hdrMailCardDAVURL: "https://dav.example.com/carddav/"}))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	if got.UID != "u9" || got.Name != "Carol" {
		t.Fatalf("path uid / name not threaded: %+v", got)
	}
}

// PUT with neither a name nor an email is rejected 400.
func TestHandleUpdateContact_EmptyRejected(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("PUT", "/v1/contacts/u9", strings.NewReader(`{"org":"ACME"}`),
		map[string]string{hdrMailCardDAVURL: "https://dav.example.com/carddav/"}))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for empty contact, got %d", resp.StatusCode)
	}
}

// PUT for a brokered account WITHOUT a CardDAV URL degrades to 501.
func TestHandleUpdateContact_NoCardDAV501(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	called := false
	orig := brokerContactPut
	brokerContactPut = func(brokerSpec, models.Contact) (models.Contact, error) {
		called = true
		return models.Contact{}, nil
	}
	t.Cleanup(func() { brokerContactPut = orig })
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("PUT", "/v1/contacts/u9",
		strings.NewReader(`{"name":"Carol"}`), nil))
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("PUT seam invoked despite missing CardDAV URL")
	}
}

// DELETE /v1/contacts/:uid routes through the bearer delete seam (204),
// forwarding the ?path= object target.
func TestHandleDeleteContact_OK(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	var gotUID, gotPath string
	orig := brokerContactDelete
	brokerContactDelete = func(spec brokerSpec, uid, objPath string) error {
		gotUID, gotPath = uid, objPath
		return nil
	}
	t.Cleanup(func() { brokerContactDelete = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/contacts/u9?path=/ab/u9.vcf", nil,
		map[string]string{hdrMailCardDAVURL: "https://dav.example.com/carddav/"}))
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if gotUID != "u9" || gotPath != "/ab/u9.vcf" {
		t.Fatalf("uid/path not threaded: %q %q", gotUID, gotPath)
	}
}

// DELETE where the CardDAV backend fails surfaces as 502.
func TestHandleDeleteContact_BackendError502(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := brokerContactDelete
	brokerContactDelete = func(brokerSpec, string, string) error { return errors.New("dav 500") }
	t.Cleanup(func() { brokerContactDelete = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/contacts/u9", nil,
		map[string]string{hdrMailCardDAVURL: "https://dav.example.com/carddav/"}))
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

// DELETE for a brokered account WITHOUT a CardDAV URL degrades to 501.
func TestHandleDeleteContact_NoCardDAV501(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	called := false
	orig := brokerContactDelete
	brokerContactDelete = func(brokerSpec, string, string) error { called = true; return nil }
	t.Cleanup(func() { brokerContactDelete = orig })
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/contacts/u9", nil, nil))
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("delete seam invoked despite missing CardDAV URL")
	}
}

// --- rules: update + footgun rejection on update + validation ---------------

// PUT /v1/rules/:id forwards the path id and the validated rule to the store.
func TestRules_Update_RoundTrip(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	rule := models.MailRule{
		Name: "Renamed", Enabled: true, Match: "all",
		Conditions: []models.RuleCondition{{Field: "from", Op: "contains", Value: "x"}},
		Actions:    []models.RuleAction{{Type: "label", Value: "X"}},
	}
	body, _ := json.Marshal(rule)
	resp, _ := app.Test(brokeredReq("PUT", "/v1/rules/r7", bytes.NewReader(body),
		map[string]string{hdrMailRulesURL: "http://rules.internal/internal/mailrules"}))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	if mock.updatedID != "r7" {
		t.Fatalf("update id = %q; want r7", mock.updatedID)
	}
}

// The footgun denylist must reject a permanent-delete action on UPDATE too,
// before the store is ever contacted.
func TestRules_Update_RejectsPermanentDelete(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	rule := models.MailRule{
		Name: "nuke", Enabled: true, Match: "all",
		Conditions: []models.RuleCondition{{Field: "from", Op: "contains", Value: "x"}},
		Actions:    []models.RuleAction{{Type: "delete_forever"}},
	}
	body, _ := json.Marshal(rule)
	resp, _ := app.Test(brokeredReq("PUT", "/v1/rules/r7", bytes.NewReader(body),
		map[string]string{hdrMailRulesURL: "http://rules.internal/internal/mailrules"}))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for delete_forever, got %d", resp.StatusCode)
	}
	if mock.updatedID != "" {
		t.Fatalf("forbidden action reached the store (updatedID=%q)", mock.updatedID)
	}
}

// Every forbidden action type must be rejected 400 by validateRuleShape.
func TestValidateRuleShape_AllForbiddenActionsRejected(t *testing.T) {
	base := models.MailRule{
		Name:       "r",
		Conditions: []models.RuleCondition{{Field: "from", Op: "contains", Value: "x"}},
	}
	for action := range models.ForbiddenRuleActions {
		r := base
		r.Actions = []models.RuleAction{{Type: action}}
		if err := validateRuleShape(r); err == nil {
			t.Fatalf("forbidden action %q was NOT rejected", action)
		}
	}
	// Mixed-case / padded forbidden action still rejected (denylist normalizes).
	r := base
	r.Actions = []models.RuleAction{{Type: "  Forward  "}}
	if err := validateRuleShape(r); err == nil {
		t.Fatalf("padded/mixed-case forbidden action not rejected")
	}
}

// validateRuleShape rejects structurally-incomplete rules with clear errors.
func TestValidateRuleShape_StructuralRejections(t *testing.T) {
	cond := []models.RuleCondition{{Field: "from", Op: "contains", Value: "x"}}
	act := []models.RuleAction{{Type: "label", Value: "L"}}

	if err := validateRuleShape(models.MailRule{Conditions: cond, Actions: act}); err == nil {
		t.Fatal("missing name not rejected")
	}
	if err := validateRuleShape(models.MailRule{Name: "n", Actions: act}); err == nil {
		t.Fatal("missing conditions not rejected")
	}
	if err := validateRuleShape(models.MailRule{Name: "n", Conditions: cond}); err == nil {
		t.Fatal("missing actions not rejected")
	}
	// A valid rule passes.
	if err := validateRuleShape(models.MailRule{Name: "n", Conditions: cond, Actions: act}); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
}

// A malformed rule JSON body is rejected 400 on both create and update.
func TestRules_MalformedJSON400(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	for _, path := range []string{"/v1/rules", "/v1/rules/r1"} {
		method := "POST"
		if strings.Contains(path, "/r1") {
			method = "PUT"
		}
		resp, _ := app.Test(brokeredReq(method, path, strings.NewReader(`{bad`),
			map[string]string{hdrMailRulesURL: "http://rules.internal/internal/mailrules"}))
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s %s: want 400 for malformed JSON, got %d", method, path, resp.StatusCode)
		}
	}
}

// --- httpRuleStore: real HTTP client against an httptest server -------------

// The brokered rule-store HTTP client must set the auth + account headers,
// target the right method/path, and decode the response — exercised end to end
// against an in-process server (no live vulos-mail).
func TestHTTPRuleStore_CRUDAgainstServer(t *testing.T) {
	var gotAuth, gotAccount, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(hdrRulesAuth)
		gotAccount = r.Header.Get(hdrRulesAccount)
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"rules":[{"id":"r1","name":"News"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reorder"):
			io.WriteString(w, `{"rules":[{"id":"b"},{"id":"a"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run"):
			io.WriteString(w, `{"matched":4,"applied":2}`)
		case r.Method == http.MethodPost:
			io.WriteString(w, `{"rule":{"id":"r_new","name":"Created"}}`)
		case r.Method == http.MethodPut:
			io.WriteString(w, `{"rule":{"id":"r1","name":"Updated"}}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	store := newRuleStore(srv.URL+"/internal/mailrules", "the-secret", "user@gmail.com")
	ctx := context.Background()

	rules, err := store.List(ctx)
	if err != nil || len(rules) != 1 || rules[0].ID != "r1" {
		t.Fatalf("List: %+v err=%v", rules, err)
	}
	if gotAuth != "the-secret" || gotAccount != "user@gmail.com" {
		t.Fatalf("auth/account headers not set: %q %q", gotAuth, gotAccount)
	}

	created, err := store.Create(ctx, models.MailRule{Name: "Created"})
	if err != nil || created.ID != "r_new" {
		t.Fatalf("Create: %+v err=%v", created, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/internal/mailrules" {
		t.Fatalf("Create targeted wrong endpoint: %s %s", gotMethod, gotPath)
	}

	updated, err := store.Update(ctx, "r1", models.MailRule{Name: "Updated"})
	if err != nil || updated.Name != "Updated" {
		t.Fatalf("Update: %+v err=%v", updated, err)
	}
	if gotPath != "/internal/mailrules/r1" {
		t.Fatalf("Update path = %q", gotPath)
	}

	if err := store.Delete(ctx, "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/internal/mailrules/r1" {
		t.Fatalf("Delete targeted wrong endpoint: %s %s", gotMethod, gotPath)
	}

	ro, err := store.Reorder(ctx, []string{"b", "a"})
	if err != nil || len(ro) != 2 || ro[0].ID != "b" {
		t.Fatalf("Reorder: %+v err=%v", ro, err)
	}

	matched, applied, err := store.Run(ctx, "INBOX", 50)
	if err != nil || matched != 4 || applied != 2 {
		t.Fatalf("Run: matched=%d applied=%d err=%v", matched, applied, err)
	}
}

// A non-2xx rule-store response must be surfaced as a *ruleAPIError carrying the
// upstream status + message, and the handler must propagate that exact status
// (e.g. a 404 stays 404, not 502).
func TestHTTPRuleStore_UpstreamErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"rule not found"}`)
	}))
	defer srv.Close()

	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := newRuleStore
	newRuleStore = func(baseURL, secret, account string) ruleStore {
		return &httpRuleStore{base: srv.URL, secret: secret, account: account, hc: srv.Client()}
	}
	t.Cleanup(func() { newRuleStore = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, _ := app.Test(brokeredReq("DELETE", "/v1/rules/missing", nil,
		map[string]string{hdrMailRulesURL: srv.URL}))
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("upstream 404 must propagate as 404, got %d", resp.StatusCode)
	}
	var out struct {
		Error string `json:"error"`
	}
	decode(t, resp.Body, &out)
	if out.Error != "rule not found" {
		t.Fatalf("upstream error message lost: %q", out.Error)
	}
}

// An unreachable rule store surfaces as 502 (rule store unreachable).
func TestHTTPRuleStore_UnreachableIs502(t *testing.T) {
	s := &httpRuleStore{base: "http://127.0.0.1:1", secret: "x", account: "a", hc: http.DefaultClient}
	_, err := s.List(context.Background())
	if err == nil {
		t.Fatal("want error for unreachable store")
	}
	var apiErr *ruleAPIError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusBadGateway {
		t.Fatalf("want *ruleAPIError 502, got %v", err)
	}
}

// --- attachment header: RFC 5987 non-ASCII filename round-trip --------------

// A non-ASCII attachment filename must be safely emitted as BOTH a quoted ASCII
// fallback and an RFC 5987 filename* form, with no CR/LF and correct percent
// encoding. This exercises contentDisposition's non-ASCII branch + rfc5987Escape.
func TestContentDisposition_NonASCIIFilename(t *testing.T) {
	cd := contentDisposition("naïve résumé.pdf")
	if strings.ContainsAny(cd, "\r\n") {
		t.Fatalf("CR/LF in Content-Disposition: %q", cd)
	}
	if !strings.Contains(cd, `filename="na_ve r_sum_.pdf"`) {
		t.Fatalf("ASCII fallback wrong: %q", cd)
	}
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("RFC 5987 form missing: %q", cd)
	}
	// The UTF-8 'ï' (0xC3 0xAF) must be percent-encoded, not literal.
	if !strings.Contains(cd, "%C3%AF") {
		t.Fatalf("non-ASCII byte not percent-encoded: %q", cd)
	}
}

// A filename made up entirely of control characters strips to empty → bare
// "attachment" with no filename param.
func TestContentDisposition_AllControlStripsToBare(t *testing.T) {
	cd := contentDisposition("\r\n\t\x00")
	if cd != "attachment" {
		t.Fatalf("want bare attachment, got %q", cd)
	}
}

// rfc5987Escape leaves attr-chars literal and percent-encodes everything else.
func TestRFC5987Escape(t *testing.T) {
	if got := rfc5987Escape("aZ0-._~"); got != "aZ0-._~" {
		t.Fatalf("attr-chars mangled: %q", got)
	}
	if got := rfc5987Escape(" "); got != "%20" {
		t.Fatalf("space escape = %q; want %%20", got)
	}
	if got := rfc5987Escape("é"); got != "%C3%A9" {
		t.Fatalf("utf-8 escape = %q; want %%C3%%A9", got)
	}
}

// Guard: the demo attachment MailClient satisfies the interface (compile-time)
// and the rich client's move/delete recording is well-formed. Ensures the test
// doubles used above match the real api.MailClient contract.
var _ api.MailClient = (*richClient)(nil)
