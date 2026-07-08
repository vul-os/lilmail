package jsonapi

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// mockThreadStore records calls and returns canned data, standing in for the
// brokered HTTP client to vulos-mail's thread store.
type mockThreadStore struct {
	account       string // set by newThreadStore substitution below (isolation assert)
	resolveIn     []string
	resolveOut    map[string]string
	conversation  []string
	summaries     []ThreadSummary
	err           error
	resolveCalled bool
	convCalled    bool
}

func (m *mockThreadStore) Resolve(_ context.Context, ids []string) (map[string]string, error) {
	m.resolveCalled = true
	m.resolveIn = ids
	return m.resolveOut, m.err
}
func (m *mockThreadStore) Conversation(_ context.Context, _ string) ([]string, error) {
	m.convCalled = true
	return m.conversation, m.err
}
func (m *mockThreadStore) List(_ context.Context, _ string) ([]ThreadSummary, error) {
	return m.summaries, m.err
}

// threadFakeClient is a MailClient whose folder/message listing is seeded so
// GET /v1/threads/:id can hydrate messages by Message-ID without a live server.
type threadFakeClient struct {
	fakeMailClient
	byFolder map[string][]models.Email
}

func (f *threadFakeClient) FetchFolders() ([]*api.MailboxInfo, error) {
	var out []*api.MailboxInfo
	for name := range f.byFolder {
		out = append(out, &api.MailboxInfo{Name: name})
	}
	return out, nil
}
func (f *threadFakeClient) FetchMessagesPaged(folder string, _ uint32, _ uint32) ([]models.Email, error) {
	return f.byFolder[folder], nil
}

// threadsApp wires a brokered /v1 app whose thread store is the given mock, and
// (optionally) a seeded mail client for the hydration path. It captures the
// account newThreadStore was built with so isolation can be asserted.
func threadsApp(t *testing.T, mock *mockThreadStore, cl api.MailClient) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")

	origStore := newThreadStore
	newThreadStore = func(baseURL, secret, account string) threadStore {
		mock.account = account
		return mock
	}
	t.Cleanup(func() { newThreadStore = origStore })

	if cl != nil {
		origDial := brokerDialIMAP
		brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
		t.Cleanup(func() { brokerDialIMAP = origDial })
	}

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

// TestThreads_ThreadedListAttachesServerThreadIDs verifies ?threaded=1 stamps the
// server thread id onto each message via the thread store's Resolve.
func TestThreads_ThreadedListAttachesServerThreadIDs(t *testing.T) {
	page := []models.Email{
		{ID: "1", MessageID: "<a@host>", Subject: "hi"},
		{ID: "2", MessageID: "<b@host>", Subject: "re: hi"},
		{ID: "3", MessageID: "<c@host>", Subject: "unknown"},
	}
	cl := &threadFakeClient{byFolder: map[string][]models.Email{"INBOX": page}}
	mock := &mockThreadStore{resolveOut: map[string]string{
		"<a@host>": "T1",
		"<b@host>": "T1",
		// c@host omitted → unknown → empty ThreadID
	}}
	app := threadsApp(t, mock, cl)

	req := httptest.NewRequest("GET", "/v1/messages?threaded=1", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailThreadsURL, "http://threads.internal/internal/mailthreads")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("threaded list: %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].ThreadID != "T1" || out.Messages[1].ThreadID != "T1" {
		t.Fatalf("thread ids not attached: %+v", out.Messages)
	}
	if out.Messages[2].ThreadID != "" {
		t.Fatalf("unknown message-id must have empty threadId, got %q", out.Messages[2].ThreadID)
	}
	if !mock.resolveCalled {
		t.Fatal("Resolve was not called")
	}
	// Isolation: the account handed to the store is the validated broker email.
	if mock.account != "user@gmail.com" {
		t.Fatalf("thread store account = %q; want validated broker email", mock.account)
	}
}

// TestThreads_ConversationHydratesMessages verifies GET /v1/threads/:id resolves
// the conversation's Message-IDs and returns those messages, date-ascending.
func TestThreads_ConversationHydratesMessages(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	inbox := []models.Email{
		{ID: "10", MessageID: "<b@host>", Subject: "reply", Date: t0.Add(time.Hour)},
		{ID: "11", MessageID: "<x@host>", Subject: "unrelated", Date: t0},
	}
	sent := []models.Email{
		{ID: "20", MessageID: "<a@host>", Subject: "orig", Date: t0},
	}
	cl := &threadFakeClient{byFolder: map[string][]models.Email{"INBOX": inbox, "Sent": sent}}
	mock := &mockThreadStore{conversation: []string{"<a@host>", "<b@host>"}}
	app := threadsApp(t, mock, cl)

	req := httptest.NewRequest("GET", "/v1/threads/T1", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailThreadsURL, "http://threads.internal/internal/mailthreads")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("conversation: %d", resp.StatusCode)
	}
	var out struct {
		ThreadID string         `json:"threadId"`
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if out.ThreadID != "T1" {
		t.Fatalf("threadId = %q", out.ThreadID)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 conversation messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	// date ascending: <a@host> (t0) before <b@host> (t0+1h)
	if out.Messages[0].MessageID != "<a@host>" || out.Messages[1].MessageID != "<b@host>" {
		t.Fatalf("messages not date-ascending: %+v", out.Messages)
	}
	if !mock.convCalled {
		t.Fatal("Conversation was not called")
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("conversation account = %q; want validated broker email", mock.account)
	}
}

// TestThreads_AccountIsValidatedNotClientSupplied asserts a client-supplied
// account query cannot override the validated broker mailbox handed to the store.
func TestThreads_AccountIsValidatedNotClientSupplied(t *testing.T) {
	cl := &threadFakeClient{byFolder: map[string][]models.Email{"INBOX": {
		{ID: "1", MessageID: "<a@host>"},
	}}}
	mock := &mockThreadStore{resolveOut: map[string]string{"<a@host>": "T1"}}
	app := threadsApp(t, mock, cl)

	// Attacker tries to read someone else's mailbox via ?account=.
	req := httptest.NewRequest("GET", "/v1/messages?threaded=1&account=victim@evil.example", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailThreadsURL, "http://threads.internal/internal/mailthreads")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if mock.account != "user@gmail.com" {
		t.Fatalf("client overrode account: store saw %q, want validated broker email", mock.account)
	}
}

// TestThreads_DegradeNoThreadStore: without a threads-store URL, ?threaded=1 still
// returns 200 with empty threadIds, and GET /v1/threads/:id returns 501.
func TestThreads_DegradeNoThreadStore(t *testing.T) {
	cl := &threadFakeClient{byFolder: map[string][]models.Email{"INBOX": {
		{ID: "1", MessageID: "<a@host>"},
	}}}
	mock := &mockThreadStore{resolveOut: map[string]string{"<a@host>": "T1"}}
	app := threadsApp(t, mock, cl)

	// threaded list, NO threads URL → 200, empty threadId, store never consulted.
	req := httptest.NewRequest("GET", "/v1/messages?threaded=1", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// deliberately NOT setting hdrMailThreadsURL
	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("degraded threaded list: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Messages []models.Email `json:"messages"`
	}
	decode(t, resp.Body, &out)
	if len(out.Messages) != 1 || out.Messages[0].ThreadID != "" {
		t.Fatalf("non-hosted account must have empty threadId: %+v", out.Messages)
	}
	if mock.resolveCalled {
		t.Fatal("thread store must NOT be consulted without a threads URL")
	}

	// GET /v1/threads/:id with no threads URL → 501.
	req2 := httptest.NewRequest("GET", "/v1/threads/T1", nil)
	req2.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req2.Header.Set(k, v)
	}
	resp2, _ := app.Test(req2)
	if resp2.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("no threads URL: GET /v1/threads/:id want 501, got %d", resp2.StatusCode)
	}
}

// TestThreads_ListSummaries exercises the optional threaded mailbox listing.
func TestThreads_ListSummaries(t *testing.T) {
	mock := &mockThreadStore{summaries: []ThreadSummary{
		{ThreadID: "T1", Count: 3, Unread: 1, LatestMessageID: "<c@host>", Subject: "hi"},
	}}
	app := threadsApp(t, mock, nil)

	req := httptest.NewRequest("GET", "/v1/threads?label=inbox", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailThreadsURL, "http://threads.internal/internal/mailthreads")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("list threads: %d", resp.StatusCode)
	}
	var out struct {
		Threads []ThreadSummary `json:"threads"`
	}
	decode(t, resp.Body, &out)
	if len(out.Threads) != 1 || out.Threads[0].ThreadID != "T1" || out.Threads[0].Count != 3 {
		t.Fatalf("list summaries = %+v", out.Threads)
	}
}
