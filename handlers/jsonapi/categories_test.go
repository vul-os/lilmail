package jsonapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

var errTestUpstream = errors.New("upstream unreachable")

// mockCategoryStore records calls and returns canned data, standing in for the
// brokered HTTP client to vulos-mail's category surface.
type mockCategoryStore struct {
	account        string // set by newCategoryStore substitution (isolation assert)
	resolveOut     map[string]string
	resolveCalled  bool
	recatCalled    bool
	recatMessageID string
	recatCategory  string
	recatTrained   bool
	err            error
}

func (m *mockCategoryStore) Resolve(_ context.Context, ids []string) (map[string]string, error) {
	m.resolveCalled = true
	return m.resolveOut, m.err
}
func (m *mockCategoryStore) Recategorize(_ context.Context, messageID, category string) (bool, error) {
	m.recatCalled = true
	m.recatMessageID = messageID
	m.recatCategory = category
	return m.recatTrained, m.err
}

// catFakeClient seeds a single INBOX page so GET /v1/messages augments/filters.
type catFakeClient struct {
	fakeMailClient
	page []models.Email
}

func (f *catFakeClient) FetchMessagesPaged(string, uint32, uint32) ([]models.Email, error) {
	return f.page, nil
}

// categoriesApp wires a brokered /v1 app whose category store is the given mock,
// plus a seeded mail client for the listing path. Captures the account
// newCategoryStore was built with so isolation can be asserted.
func categoriesApp(t *testing.T, mock *mockCategoryStore, page []models.Email) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	orig := newCategoryStore
	newCategoryStore = func(baseURL, secret, account string) categoryStore {
		mock.account = account
		return mock
	}
	t.Cleanup(func() { newCategoryStore = orig })

	cl := &catFakeClient{page: page}
	origDial := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
	t.Cleanup(func() { brokerDialIMAP = origDial })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

func jsonBody(s string) *strings.Reader { return strings.NewReader(s) }

// TestCategories_ListStampsCategory verifies the message listing stamps each
// message with its category via the store's Resolve.
func TestCategories_ListStampsCategory(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>", Subject: "hi"},
		{ID: "2", MessageID: "<b@host>", Subject: "sale"},
		{ID: "3", MessageID: "<c@host>", Subject: "unknown"},
	}
	mock := &mockCategoryStore{resolveOut: map[string]string{
		"<a@host>": "primary",
		"<b@host>": "promotions",
		// c@host omitted → left empty
	}}
	app := categoriesApp(t, mock, page)

	req := httptest.NewRequest("GET", "/v1/messages", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 3 {
		t.Fatalf("want 3, got %d", len(out.Messages))
	}
	if out.Messages[0].Category != "primary" || out.Messages[1].Category != "promotions" {
		t.Fatalf("categories not stamped: %+v", out.Messages)
	}
	if out.Messages[2].Category != "" {
		t.Fatalf("unknown message must have empty category, got %q", out.Messages[2].Category)
	}
	if !mock.resolveCalled {
		t.Fatal("Resolve not called")
	}
	// Isolation: the account handed to the store is the validated broker email.
	if mock.account != "user@gmail.com" {
		t.Fatalf("category store account = %q; want validated broker email", mock.account)
	}
}

// TestCategories_FilterByCategory verifies ?category= filters the page to one tab.
func TestCategories_FilterByCategory(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
		{ID: "3", MessageID: "<c@host>"},
	}
	mock := &mockCategoryStore{resolveOut: map[string]string{
		"<a@host>": "primary",
		"<b@host>": "promotions",
		"<c@host>": "promotions",
	}}
	app := categoriesApp(t, mock, page)

	req := httptest.NewRequest("GET", "/v1/messages?category=promotions", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("filtered list: %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 promotions, got %d: %+v", len(out.Messages), out.Messages)
	}
	for _, m := range out.Messages {
		if m.Category != "promotions" {
			t.Fatalf("filter leaked a non-promotions message: %+v", m)
		}
	}
}

// TestCategories_RecategorizeTrains verifies POST /v1/messages/:uid/category
// re-categorizes and reports training.
func TestCategories_RecategorizeTrains(t *testing.T) {
	mock := &mockCategoryStore{recatTrained: true}
	app := categoriesApp(t, mock, nil)

	req := httptest.NewRequest("POST", "/v1/messages/2/category", jsonBody(`{"category":"promotions","messageId":"<b@host>"}`))
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("recategorize: %d", resp.StatusCode)
	}
	var out struct {
		OK       bool   `json:"ok"`
		Category string `json:"category"`
		Trained  bool   `json:"trained"`
	}
	decode(t, resp.Body, &out)
	if !out.OK || out.Category != "promotions" || !out.Trained {
		t.Fatalf("recategorize response = %+v", out)
	}
	if !mock.recatCalled || mock.recatCategory != "promotions" || mock.recatMessageID != "<b@host>" {
		t.Fatalf("store not called correctly: %+v", mock)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("recategorize account = %q; want validated broker email", mock.account)
	}
}

// TestCategories_RecategorizeRejectsInvalid rejects an out-of-enum category
// (untrusted input validated before it reaches the training seam).
func TestCategories_RecategorizeRejectsInvalid(t *testing.T) {
	mock := &mockCategoryStore{}
	app := categoriesApp(t, mock, nil)

	for _, bad := range []string{`{"category":"spam","messageId":"<b@host>"}`, `{"category":"../etc","messageId":"<b@host>"}`, `{"messageId":"<b@host>"}`} {
		req := httptest.NewRequest("POST", "/v1/messages/2/category", jsonBody(bad))
		req.Header.Set(hdrBrokerAuth, "s3cr3t")
		req.Header.Set("Content-Type", "application/json")
		for k, v := range brokeredHeaders() {
			req.Header.Set(k, v)
		}
		req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

		resp, _ := app.Test(req)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("bad category %q: want 400, got %d", bad, resp.StatusCode)
		}
	}
	if mock.recatCalled {
		t.Fatal("store must NOT be called for an invalid category")
	}
}

// TestCategories_DegradeNoCategoryStore: without a category URL, the listing
// returns 200 with empty categories (single Primary tab), ?category= is ignored,
// and the re-categorize endpoint returns 501.
func TestCategories_DegradeNoCategoryStore(t *testing.T) {
	page := []models.Email{{ID: "1", MessageID: "<a@host>"}}
	mock := &mockCategoryStore{resolveOut: map[string]string{"<a@host>": "promotions"}}
	app := categoriesApp(t, mock, page)

	// list with ?category= but NO category URL → 200, empty category, store not consulted.
	req := httptest.NewRequest("GET", "/v1/messages?category=promotions", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// deliberately NOT setting hdrMailCategoriesURL
	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("degraded list: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 1 || out.Messages[0].Category != "" {
		t.Fatalf("non-hosted account must have empty category and be unfiltered: %+v", out.Messages)
	}
	if mock.resolveCalled {
		t.Fatal("category store must NOT be consulted without a category URL")
	}

	// re-categorize with no category URL → 501.
	req2 := httptest.NewRequest("POST", "/v1/messages/1/category", jsonBody(`{"category":"promotions","messageId":"<a@host>"}`))
	req2.Header.Set(hdrBrokerAuth, "s3cr3t")
	req2.Header.Set("Content-Type", "application/json")
	for k, v := range brokeredHeaders() {
		req2.Header.Set(k, v)
	}
	resp2, _ := app.Test(req2)
	if resp2.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("no category URL: recategorize want 501, got %d", resp2.StatusCode)
	}
}

// TestCategories_ResolveErrorDoesNotFilter: when the category store is present
// but Resolve FAILS (upstream unreachable), the listing must NOT filter — it
// returns the full, un-augmented page rather than an empty tab (never hide mail).
func TestCategories_ResolveErrorDoesNotFilter(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
	}
	mock := &mockCategoryStore{err: errTestUpstream}
	app := categoriesApp(t, mock, page)

	req := httptest.NewRequest("GET", "/v1/messages?category=social", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("resolve-error list: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	// Full page returned (not filtered to empty) despite the ?category=social.
	if len(out.Messages) != 2 {
		t.Fatalf("resolve error must not filter; want full page of 2, got %d", len(out.Messages))
	}
}

// TestCategories_NextOffsetFromPreFilterLength: paging must be driven by the raw
// IMAP page fullness, not the (smaller) filtered count — otherwise a category tab
// stops paging after the first page and loses mail beyond it.
func TestCategories_NextOffsetFromPreFilterLength(t *testing.T) {
	// A FULL page (limit=2) where only one message is in the wanted category.
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>"},
		{ID: "2", MessageID: "<b@host>"},
	}
	mock := &mockCategoryStore{resolveOut: map[string]string{
		"<a@host>": "social",
		"<b@host>": "primary",
	}}
	app := categoriesApp(t, mock, page)

	req := httptest.NewRequest("GET", "/v1/messages?category=social&limit=2", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var out struct {
		Messages   []models.Email `json:"messages"`
		NextOffset interface{}    `json:"nextOffset"`
	}
	decode(t, resp.Body, &out)
	// Filtered to 1 Social message...
	if len(out.Messages) != 1 {
		t.Fatalf("want 1 filtered message, got %d", len(out.Messages))
	}
	// ...but nextOffset must be set (the raw page was FULL at limit=2), so the
	// client keeps paging to find more Social mail.
	if out.NextOffset == nil {
		t.Fatalf("nextOffset must be set from the full raw page, not the filtered count")
	}
}

// TestCategories_AccountIsValidatedNotClientSupplied asserts a client-supplied
// account query cannot override the validated broker mailbox handed to the store.
func TestCategories_AccountIsValidatedNotClientSupplied(t *testing.T) {
	page := []models.Email{{ID: "1", MessageID: "<a@host>"}}
	mock := &mockCategoryStore{resolveOut: map[string]string{"<a@host>": "primary"}}
	app := categoriesApp(t, mock, page)

	req := httptest.NewRequest("GET", "/v1/messages?account=victim@evil.example", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCategoriesURL, "http://cat.internal/internal/categories")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("client overrode account: store saw %q, want validated broker email", mock.account)
	}
}
