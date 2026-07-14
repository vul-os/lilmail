package jsonapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// mockSmartFolderStore records calls and returns canned data, standing in for the
// brokered HTTP client to vulos-mail's smart-folder surface.
type mockSmartFolderStore struct {
	account       string // captured by newSmartFolderStore substitution (isolation)
	resolveOut    map[string]string
	resolveCalled bool
	refileCalled  bool
	refileMsgID   string
	refileFolder  string
	refileTrained bool
	fieldsOut     *models.SmartFields
	fieldsFound   bool
	fieldsCalled  bool
	err           error
}

func (m *mockSmartFolderStore) Resolve(_ context.Context, ids []string) (map[string]string, error) {
	m.resolveCalled = true
	return m.resolveOut, m.err
}
func (m *mockSmartFolderStore) Refile(_ context.Context, messageID, folder string) (bool, error) {
	m.refileCalled = true
	m.refileMsgID = messageID
	m.refileFolder = folder
	return m.refileTrained, m.err
}
func (m *mockSmartFolderStore) Fields(_ context.Context, messageID string) (*models.SmartFields, bool, error) {
	m.fieldsCalled = true
	return m.fieldsOut, m.fieldsFound, m.err
}

// sfFakeClient seeds a single INBOX page + single-message read.
type sfFakeClient struct {
	fakeMailClient
	page   []models.Email
	single models.Email
}

func (f *sfFakeClient) FetchMessagesPaged(string, uint32, uint32) ([]models.Email, error) {
	return f.page, nil
}
func (f *sfFakeClient) FetchSingleMessage(string, string) (models.Email, error) {
	return f.single, nil
}

func smartFoldersApp(t *testing.T, mock *mockSmartFolderStore, page []models.Email, single models.Email) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	orig := newSmartFolderStore
	newSmartFolderStore = func(baseURL, secret, account string) smartFolderStore {
		mock.account = account
		return mock
	}
	t.Cleanup(func() { newSmartFolderStore = orig })

	cl := &sfFakeClient{page: page, single: single}
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

const hdrSmartURL = "X-Vulos-Mail-Smartfolders-Url"

// TestSmartFolders_ListStampsFolder verifies the listing stamps each message's
// smartFolder via Resolve (an un-filed message stays empty).
func TestSmartFolders_ListStampsFolder(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
		{ID: "3", MessageID: "<c@host>"},
	}
	mock := &mockSmartFolderStore{resolveOut: map[string]string{
		"<a@host>": "bills",
		"<b@host>": "shipping",
		// c omitted → empty
	}}
	app := smartFoldersApp(t, mock, page, models.Email{})

	req := httptest.NewRequest("GET", "/v1/messages", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if out.Messages[0].SmartFolder != "bills" || out.Messages[1].SmartFolder != "shipping" {
		t.Fatalf("smart folders not stamped: %+v", out.Messages)
	}
	if out.Messages[2].SmartFolder != "" {
		t.Fatalf("unfiled message must have empty smartFolder, got %q", out.Messages[2].SmartFolder)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("store account = %q; want validated broker email", mock.account)
	}
}

// TestSmartFolders_FilterByFolder verifies ?smartFolder= filters the page.
func TestSmartFolders_FilterByFolder(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
		{ID: "3", MessageID: "<c@host>"},
	}
	mock := &mockSmartFolderStore{resolveOut: map[string]string{
		"<a@host>": "bills",
		"<b@host>": "shipping",
		"<c@host>": "shipping",
	}}
	app := smartFoldersApp(t, mock, page, models.Email{})

	req := httptest.NewRequest("GET", "/v1/messages?smartFolder=shipping", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 shipping, got %d: %+v", len(out.Messages), out.Messages)
	}
	for _, m := range out.Messages {
		if m.SmartFolder != "shipping" {
			t.Fatalf("filter leaked a non-shipping message: %+v", m)
		}
	}
}

// TestSmartFolders_RefileTrains verifies POST /v1/messages/:uid/smartfolder.
func TestSmartFolders_RefileTrains(t *testing.T) {
	mock := &mockSmartFolderStore{refileTrained: true}
	app := smartFoldersApp(t, mock, nil, models.Email{})

	req := httptest.NewRequest("POST", "/v1/messages/2/smartfolder", jsonBody(`{"folder":"bills","messageId":"<b@host>"}`))
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("refile: %d", resp.StatusCode)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Folder  string `json:"folder"`
		Trained bool   `json:"trained"`
	}
	decode(t, resp.Body, &out)
	if !out.OK || out.Folder != "bills" || !out.Trained {
		t.Fatalf("refile response = %+v", out)
	}
	if !mock.refileCalled || mock.refileFolder != "bills" || mock.refileMsgID != "<b@host>" {
		t.Fatalf("store not called correctly: %+v", mock)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("refile account = %q; want validated broker email", mock.account)
	}
}

// TestSmartFolders_RefileClear accepts an empty folder = clear the label.
func TestSmartFolders_RefileClear(t *testing.T) {
	mock := &mockSmartFolderStore{refileTrained: true}
	app := smartFoldersApp(t, mock, nil, models.Email{})

	req := httptest.NewRequest("POST", "/v1/messages/2/smartfolder", jsonBody(`{"folder":"","messageId":"<b@host>"}`))
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("clear refile: %d", resp.StatusCode)
	}
	if !mock.refileCalled || mock.refileFolder != "" {
		t.Fatalf("clear should call store with empty folder: %+v", mock)
	}
}

// TestSmartFolders_RefileRejectsInvalid rejects an out-of-enum folder.
func TestSmartFolders_RefileRejectsInvalid(t *testing.T) {
	mock := &mockSmartFolderStore{}
	app := smartFoldersApp(t, mock, nil, models.Email{})

	for _, bad := range []string{`{"folder":"spam","messageId":"<b@host>"}`, `{"folder":"../etc","messageId":"<b@host>"}`, `{"folder":"bills"}`} {
		req := httptest.NewRequest("POST", "/v1/messages/2/smartfolder", jsonBody(bad))
		req.Header.Set(hdrBrokerAuth, "s3cr3t")
		req.Header.Set("Content-Type", "application/json")
		for k, v := range brokeredHeaders() {
			req.Header.Set(k, v)
		}
		req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")
		resp, _ := app.Test(req)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("bad folder %q: want 400, got %d", bad, resp.StatusCode)
		}
	}
	if mock.refileCalled {
		t.Fatal("store must NOT be called for an invalid folder")
	}
}

// TestSmartFolders_MessageCarriesFields verifies the single-message read stamps
// smartFields from the store.
func TestSmartFolders_MessageCarriesFields(t *testing.T) {
	mock := &mockSmartFolderStore{
		fieldsFound: true,
		fieldsOut:   &models.SmartFields{Tracking: "1Z999", Carrier: "UPS"},
	}
	single := models.Email{ID: "5", MessageID: "<x@host>", Subject: "shipped"}
	app := smartFoldersApp(t, mock, nil, single)

	req := httptest.NewRequest("GET", "/v1/messages/5", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	var out models.Email
	decode(t, resp.Body, &out)
	if out.SmartFields == nil || out.SmartFields.Tracking != "1Z999" || out.SmartFields.Carrier != "UPS" {
		t.Fatalf("smartFields not stamped: %+v", out.SmartFields)
	}
	if !mock.fieldsCalled {
		t.Fatal("Fields not called")
	}
}

// TestSmartFolders_DegradeNoStore: no smart-folders URL → 200 empty smartFolder,
// ?smartFolder= ignored, refile 501, no store consulted.
func TestSmartFolders_DegradeNoStore(t *testing.T) {
	page := []models.Email{{ID: "1", MessageID: "<a@host>"}}
	mock := &mockSmartFolderStore{resolveOut: map[string]string{"<a@host>": "bills"}}
	app := smartFoldersApp(t, mock, page, models.Email{})

	req := httptest.NewRequest("GET", "/v1/messages?smartFolder=bills", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// deliberately NOT setting hdrSmartURL
	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("degraded list: %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 1 || out.Messages[0].SmartFolder != "" {
		t.Fatalf("non-hosted must be empty smartFolder + unfiltered: %+v", out.Messages)
	}
	if mock.resolveCalled {
		t.Fatal("store must NOT be consulted without a smart-folders URL")
	}

	req2 := httptest.NewRequest("POST", "/v1/messages/1/smartfolder", jsonBody(`{"folder":"bills","messageId":"<a@host>"}`))
	req2.Header.Set(hdrBrokerAuth, "s3cr3t")
	req2.Header.Set("Content-Type", "application/json")
	for k, v := range brokeredHeaders() {
		req2.Header.Set(k, v)
	}
	resp2, _ := app.Test(req2)
	if resp2.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("no smart-folders URL: refile want 501, got %d", resp2.StatusCode)
	}
}

// TestSmartFolders_ResolveErrorDoesNotFilter: a Resolve error must not filter the
// list to empty (never hide mail).
func TestSmartFolders_ResolveErrorDoesNotFilter(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
	}
	mock := &mockSmartFolderStore{err: errTestUpstream}
	app := smartFoldersApp(t, mock, page, models.Email{})

	req := httptest.NewRequest("GET", "/v1/messages?smartFolder=bills", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 2 {
		t.Fatalf("resolve error must not filter; want full page of 2, got %d", len(out.Messages))
	}
}

// TestSmartFolders_AccountValidatedNotClient asserts client account query cannot
// override the validated broker mailbox.
func TestSmartFolders_AccountValidatedNotClient(t *testing.T) {
	page := []models.Email{{ID: "1", MessageID: "<a@host>"}}
	mock := &mockSmartFolderStore{resolveOut: map[string]string{"<a@host>": "bills"}}
	app := smartFoldersApp(t, mock, page, models.Email{})

	req := httptest.NewRequest("GET", "/v1/messages?account=victim@evil.example", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrSmartURL, "http://sf.internal/internal/smartfolders")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("client overrode account: store saw %q, want validated broker email", mock.account)
	}
}
