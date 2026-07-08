// handlers/jsonapi/threads.go — /v1/threads: server-side conversation threading.
//
// SEAM: lilmail does not own conversation state. vulos-mail computes CANONICAL
// per-account thread ids at ingest (where delivery runs, so it sees every
// message across every folder) and exposes them via broker-gated internal HTTP
// endpoints. In a CP-brokered deployment vulos-mail injects the thread-store
// base URL as X-Vulos-Mail-Threads-Url; these handlers broker reads to it,
// presenting the shared broker secret as X-Vulos-Broker-Auth and the validated
// mailbox as the `account` field/query.
//
// HONEST DEGRADE: when no thread-store URL is brokered (a plain Gmail/IMAP
// account, or standalone/session lilmail), the surface reports 501 for
// GET /v1/threads/:id and ?threaded=1 simply leaves each message's ThreadID
// empty — the client falls back to its own JWZ union-find over the threading
// headers. A non-hosted account is never errored, only un-augmented.
//
// ISOLATION: `account` for every thread call is ALWAYS the validated broker
// mailbox (spec.Email) — never client input. A caller can only read the threads
// of THEIR OWN mailbox. Unknown/unowned thread ids return an empty conversation
// upstream, so there is no cross-account leak.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// ThreadSummary is one row of a threaded mailbox listing (GET /v1/threads),
// mirroring vulos-mail's /list response shape.
type ThreadSummary struct {
	ThreadID        string   `json:"threadId"`
	Count           int      `json:"count"`
	Unread          int      `json:"unread"`
	LatestMessageID string   `json:"latestMessageId"`
	Participants    []string `json:"participants"`
	Subject         string   `json:"subject"`
}

// threadStore is the seam the thread handlers use. The brokered HTTP client
// (httpThreadStore) satisfies it in production; tests substitute a mock via
// newThreadStore.
type threadStore interface {
	// Resolve maps each of the given Message-IDs to its canonical server thread
	// id. Unknown ids are OMITTED from the returned map.
	Resolve(ctx context.Context, messageIDs []string) (map[string]string, error)
	// Conversation returns every Message-ID header in the thread, across all
	// folders. An unknown/unowned thread id returns an empty list (no error).
	Conversation(ctx context.Context, threadID string) ([]string, error)
	// List returns thread summaries for a label, newest-active first.
	List(ctx context.Context, label string) ([]ThreadSummary, error)
}

// errThreadsUnsupported is surfaced as 501 when the mailbox backend computes no
// canonical thread ids (no X-Vulos-Mail-Threads-Url brokered for this request).
var errThreadsUnsupported = errors.New("threads are not supported by this mailbox backend")

// Bounds. The resolve batch sent upstream is capped so a large page can't drive
// an unbounded request; the response readers are capped so a hostile/broken
// upstream can't exhaust memory.
const (
	maxResolveBatch    = 1000    // vulos-mail's documented per-call max
	maxThreadRespBytes = 4 << 20 // 4 MiB cap on a /conversation or /list body
	maxThreadMessages  = 500     // cap messages materialised for one conversation
)

// newThreadStore builds the brokered HTTP thread store client. Package var so
// tests can substitute a mock without a live vulos-mail.
var newThreadStore = func(baseURL, secret, account string) threadStore {
	return &httpThreadStore{
		base:    strings.TrimRight(baseURL, "/"),
		secret:  secret,
		account: account,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// threadStoreFor resolves the thread store for a request. Threading requires the
// brokered path (the thread-store URL arrives per request); a session-only or
// non-hosted request has no thread store and gets errThreadsUnsupported.
func (h *Handler) threadStoreFor(c *fiber.Ctx) (threadStore, error) {
	spec, ok := brokerSpecOf(c)
	if !ok || strings.TrimSpace(spec.ThreadsURL) == "" {
		return nil, errThreadsUnsupported
	}
	// account is ALWAYS the validated broker mailbox, never client input.
	return newThreadStore(spec.ThreadsURL, h.brokerSecret, spec.Email), nil
}

// registerThreads mounts the /v1/threads routes on the group. Called from
// Register only when the broker path is active.
func (h *Handler) registerThreads(g fiber.Router) {
	g.Get("/threads", h.handleListThreads) // ?label=  threaded mailbox listing
	g.Get("/threads/:id", h.handleThread)  // whole conversation, messages hydrated over IMAP
}

// handleListThreads returns thread summaries for a label (default "inbox") from
// the brokered thread store. 501 when the account has no thread store.
func (h *Handler) handleListThreads(c *fiber.Ctx) error {
	store, err := h.threadStoreFor(c)
	if err != nil {
		return failThreads(c, err)
	}
	label := strings.TrimSpace(c.Query("label"))
	if label == "" {
		label = "inbox"
	}
	summaries, err := store.List(c.Context(), label)
	if err != nil {
		return failThreads(c, err)
	}
	return c.JSON(fiber.Map{"threads": nonNilThreads(summaries)})
}

// handleThread returns a whole conversation: it resolves the thread's Message-ID
// headers via the brokered store, then hydrates each message's full data over
// IMAP, returning them sorted by date ascending.
// GET /v1/threads/:id
func (h *Handler) handleThread(c *fiber.Ctx) error {
	threadID := c.Params("id")

	store, err := h.threadStoreFor(c)
	if err != nil {
		return failThreads(c, err)
	}

	msgIDs, err := store.Conversation(c.Context(), threadID)
	if err != nil {
		return failThreads(c, err)
	}
	// Unknown/unowned thread → empty conversation (no leak, not an error).
	wanted := normalizeMessageIDSet(msgIDs)
	if len(wanted) == 0 {
		return c.JSON(fiber.Map{"threadId": threadID, "messages": []models.Email{}})
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	msgs := hydrateByMessageID(cl, wanted)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Date.Before(msgs[j].Date) })
	if len(msgs) > maxThreadMessages {
		msgs = msgs[:maxThreadMessages]
	}
	return c.JSON(fiber.Map{"threadId": threadID, "messages": nonNilEmails(msgs)})
}

// attachThreadIDs resolves the canonical server thread id for each message in the
// page (by its Message-ID header) and stamps it onto emails[i].ThreadID. Messages
// whose Message-ID the store does not know are left with an empty ThreadID. A
// store error is swallowed (best-effort augmentation): the flat list is still
// returned un-augmented rather than failing the whole request. Emails is mutated
// in place. The batch is bounded inside the store's Resolve.
func (h *Handler) attachThreadIDs(ctx context.Context, store threadStore, emails []models.Email) {
	ids := make([]string, 0, len(emails))
	for i := range emails {
		if mid := strings.TrimSpace(emails[i].MessageID); mid != "" {
			ids = append(ids, mid)
		}
	}
	if len(ids) == 0 {
		return
	}
	threads, err := store.Resolve(ctx, ids)
	if err != nil || len(threads) == 0 {
		return
	}
	// Build a normalized lookup so "<a@b>" and "a@b" both match, regardless of
	// which form vulos-mail echoes back as the map key.
	norm := make(map[string]string, len(threads))
	for k, v := range threads {
		norm[normalizeMessageID(k)] = v
	}
	for i := range emails {
		key := normalizeMessageID(emails[i].MessageID)
		if key == "" {
			continue
		}
		if tid, ok := norm[key]; ok {
			emails[i].ThreadID = tid
		}
	}
}

// hydrateByMessageID fetches the full data of the messages whose Message-ID is in
// `wanted`, by scanning the account's folders. It reuses only existing MailClient
// methods (FetchFolders + FetchMessagesPaged) so it works uniformly against the
// real IMAP client, the demo client, and test mocks. Cost is bounded: at most
// maxThreadFolders folders and maxThreadScan messages per folder are examined,
// and the scan stops early once every wanted id is found.
const (
	maxThreadFolders = 25
	maxThreadScan    = 2000
)

func hydrateByMessageID(cl api.MailClient, wanted map[string]bool) []models.Email {
	remaining := len(wanted)
	seen := map[string]bool{}
	var out []models.Email

	folders, err := cl.FetchFolders()
	if err != nil {
		return out
	}
	for fi, f := range folders {
		if fi >= maxThreadFolders || remaining == 0 {
			break
		}
		if f == nil || strings.TrimSpace(f.Name) == "" {
			continue
		}
		emails, err := cl.FetchMessagesPaged(f.Name, maxThreadScan, 0)
		if err != nil {
			continue
		}
		for i := range emails {
			key := normalizeMessageID(emails[i].MessageID)
			if key == "" || !wanted[key] || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, emails[i])
			remaining--
			if remaining == 0 {
				break
			}
		}
	}
	return out
}

// failThreads maps a thread-store error to an HTTP response: unsupported → 501,
// anything else → 502.
func failThreads(c *fiber.Ctx, err error) error {
	if errors.Is(err, errThreadsUnsupported) {
		return fail(c, fiber.StatusNotImplemented, err.Error())
	}
	return fail(c, fiber.StatusBadGateway, "thread store unavailable")
}

func nonNilThreads(t []ThreadSummary) []ThreadSummary {
	if t == nil {
		return []ThreadSummary{}
	}
	return t
}

func nonNilEmails(e []models.Email) []models.Email {
	if e == nil {
		return []models.Email{}
	}
	return e
}

// normalizeMessageID lowercases and strips surrounding angle brackets + spaces so
// "<a@b>" and "a@b" compare equal. Empty stays empty.
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.ToLower(strings.TrimSpace(id))
}

// normalizeMessageIDSet builds a lookup set of normalized Message-IDs, dropping
// blanks and duplicates.
func normalizeMessageIDSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if n := normalizeMessageID(id); n != "" {
			set[n] = true
		}
	}
	return set
}

// --- httpThreadStore: brokered HTTP client to vulos-mail's thread store -------

type httpThreadStore struct {
	base    string // e.g. http://127.0.0.1:2080/internal/mailthreads
	secret  string // shared broker secret → X-Vulos-Broker-Auth
	account string // brokered mailbox → account field/query (never client input)
	hc      *http.Client
}

func (s *httpThreadStore) Resolve(ctx context.Context, messageIDs []string) (map[string]string, error) {
	if len(messageIDs) == 0 {
		return map[string]string{}, nil
	}
	// Bound the batch sent upstream.
	if len(messageIDs) > maxResolveBatch {
		messageIDs = messageIDs[:maxResolveBatch]
	}
	body := map[string]any{"account": s.account, "messageIds": messageIDs}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/resolve", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Threads map[string]string `json:"threads"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	if out.Threads == nil {
		out.Threads = map[string]string{}
	}
	return out.Threads, nil
}

func (s *httpThreadStore) Conversation(ctx context.Context, threadID string) ([]string, error) {
	// threadID and account go into the query string and MUST be url-escaped so a
	// value containing '&'/'='/etc. cannot inject extra parameters.
	q := url.Values{}
	q.Set("account", s.account)
	q.Set("id", threadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/conversation?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	var out struct {
		MessageIDs []string `json:"messageIds"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	return out.MessageIDs, nil
}

func (s *httpThreadStore) List(ctx context.Context, label string) ([]ThreadSummary, error) {
	q := url.Values{}
	q.Set("account", s.account)
	if label != "" {
		q.Set("label", label)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/list?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	var out struct {
		Threads []ThreadSummary `json:"threads"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	return out.Threads, nil
}

// doJSON performs the request and decodes a JSON response into out. The body is
// read through a LimitReader so a hostile/broken upstream cannot exhaust memory.
// Any non-2xx or transport error becomes a generic thread-store error (502).
func (s *httpThreadStore) doJSON(req *http.Request, out any) error {
	resp, err := s.hc.Do(req)
	if err != nil {
		return errors.New("thread store unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxThreadRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("thread store error")
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return errors.New("invalid thread store response")
		}
	}
	return nil
}
