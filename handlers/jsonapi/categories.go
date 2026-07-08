// handlers/jsonapi/categories.go — /v1 inbox CATEGORIES (Gmail-style tabs).
//
// SEAM: lilmail does not own classification. vulos-mail classifies each inbound
// message into ONE category tab (Primary/Social/Promotions/Updates/Forums) at
// ingest (where delivery runs) and exposes per-message categories + a training
// signal via broker-gated internal HTTP endpoints. In a CP-brokered deployment
// vulos-mail injects the category base URL as X-Vulos-Mail-Categories-Url; these
// handlers broker to it, presenting the shared broker secret as
// X-Vulos-Broker-Auth and the validated mailbox as the `account` field.
//
// SURFACE:
//   - GET /v1/messages?category=<cat>  filters the inbox listing to one tab
//     (augment-then-filter; see handleMessages). Also, each message on any
//     listing is stamped with its `category` (additive, like threadId).
//   - POST /v1/messages/:uid/category  body {category}  re-categorizes the message
//     to a tab AND trains the per-account model (mirrors the spam move-trains
//     signal). It also moves the message on IMAP when the tab maps to a folder is
//     NOT done here — categories are labels inside the inbox, not folders, so the
//     re-categorization is purely the trained category state.
//
// HONEST DEGRADE: when no category URL is brokered (a plain Gmail/IMAP account,
// or standalone/session lilmail), the listing leaves each `category` empty — the
// client shows a SINGLE Primary tab — `?category=` is ignored, and the
// re-categorize endpoint reports 501. A non-hosted account is never errored,
// only un-tabbed.
//
// ISOLATION: `account` for every category call is ALWAYS the validated broker
// mailbox (spec.Email) — never client input. A caller can only read/train the
// categories of THEIR OWN mailbox. Training + model are per-account upstream.
//
// ROBUSTNESS: the target category is validated against the fixed five-value enum
// before it is sent upstream; the resolve batch and response bodies are bounded;
// classification runs on UNTRUSTED headers upstream (bounded, no-ReDoS, fail-safe
// to Primary), and the category value is a plain enum token — never rendered as
// raw untrusted header content.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// knownCategories is the fixed set of inbox tabs. Re-categorization input is
// validated against it (case-insensitive) so a caller can never inject an
// arbitrary label into the trained model or the IMAP state.
var knownCategories = map[string]bool{
	"primary": true, "social": true, "promotions": true, "updates": true, "forums": true,
}

// normalizeCategory lowercases/trims a category token and reports whether it is
// one of the five known tabs.
func normalizeCategory(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, knownCategories[s]
}

// categoryStore is the seam the category handlers use. The brokered HTTP client
// (httpCategoryStore) satisfies it in production; tests substitute a mock via
// newCategoryStore.
type categoryStore interface {
	// Resolve maps each of the given Message-IDs to its category tab. A message
	// with no explicit category reports "primary"; unknown ids are OMITTED.
	Resolve(ctx context.Context, messageIDs []string) (map[string]string, error)
	// Recategorize moves the message (by Message-ID header) to `category` and
	// trains the per-account model. Returns whether the training half succeeded.
	Recategorize(ctx context.Context, messageID, category string) (bool, error)
}

// errCategoriesUnsupported is surfaced as 501 when the mailbox backend does not
// classify mail into tabs (no X-Vulos-Mail-Categories-Url brokered).
var errCategoriesUnsupported = errors.New("categories are not supported by this mailbox backend")

// Bounds mirror the threads surface: the resolve batch is capped upstream and the
// response readers are capped so a hostile/broken upstream can't exhaust memory.
const maxCategoryRespBytes = 4 << 20 // 4 MiB

// newCategoryStore builds the brokered HTTP category store client. Package var so
// tests can substitute a mock without a live vulos-mail.
var newCategoryStore = func(baseURL, secret, account string) categoryStore {
	return &httpCategoryStore{
		base:    strings.TrimRight(baseURL, "/"),
		secret:  secret,
		account: account,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// categoryStoreFor resolves the category store for a request. Categories require
// the brokered path (the category URL arrives per request); a session-only or
// non-hosted request has no category store and gets errCategoriesUnsupported.
func (h *Handler) categoryStoreFor(c *fiber.Ctx) (categoryStore, error) {
	spec, ok := brokerSpecOf(c)
	if !ok || strings.TrimSpace(spec.CategoriesURL) == "" {
		return nil, errCategoriesUnsupported
	}
	// account is ALWAYS the validated broker mailbox, never client input.
	return newCategoryStore(spec.CategoriesURL, h.brokerSecret, spec.Email), nil
}

// registerCategories mounts the /v1 category routes. Called from Register only
// when the broker path is active.
func (h *Handler) registerCategories(g fiber.Router) {
	g.Post("/messages/:uid/category", h.handleRecategorize) // body {category, messageId?}
}

// attachCategories resolves each message's category tab (by its Message-ID
// header) and stamps it onto emails[i].Category. Messages the store does not know
// are left empty. A store error is swallowed (best-effort augmentation): the list
// is returned un-augmented rather than failing the whole request. Emails is
// mutated in place. Mirrors attachThreadIDs.
//
// Returns true only when the resolve actually SUCCEEDED and returned a non-empty
// result — i.e. the categories are authoritative for this page. The caller uses
// this to decide whether ?category= filtering is safe: a false return (upstream
// unreachable, or nothing resolved) must NOT filter, or a transient error would
// hide a real message list behind the empty "primary" default.
func (h *Handler) attachCategories(ctx context.Context, store categoryStore, emails []models.Email) bool {
	ids := make([]string, 0, len(emails))
	for i := range emails {
		if mid := strings.TrimSpace(emails[i].MessageID); mid != "" {
			ids = append(ids, mid)
		}
	}
	if len(ids) == 0 {
		return false
	}
	cats, err := store.Resolve(ctx, ids)
	if err != nil || len(cats) == 0 {
		return false
	}
	norm := make(map[string]string, len(cats))
	for k, v := range cats {
		if cv, ok := normalizeCategory(v); ok {
			norm[normalizeMessageID(k)] = cv
		}
	}
	for i := range emails {
		key := normalizeMessageID(emails[i].MessageID)
		if key == "" {
			continue
		}
		if cat, ok := norm[key]; ok {
			emails[i].Category = cat
		}
	}
	return true
}

// filterByCategory returns the subset of emails whose Category equals want
// (case-insensitive). Called only when ?category= is present AND a category store
// augmented the page. NOTE: this filters the CURRENT PAGE only — pagination stays
// server-authoritative on the flat listing, so a category tab may show fewer than
// `limit` rows per page; the client keeps paging. This is documented behaviour,
// honest about the seam (vulos-mail owns classification, lilmail lists over IMAP).
func filterByCategory(emails []models.Email, want string) []models.Email {
	want = strings.ToLower(strings.TrimSpace(want))
	out := emails[:0]
	for _, e := range emails {
		ec := e.Category
		if ec == "" {
			ec = "primary" // an un-stamped message is Primary by definition (fail-safe)
		}
		if strings.EqualFold(ec, want) {
			out = append(out, e)
		}
	}
	return out
}

// handleRecategorize re-categorizes a message to a tab and trains the per-account
// model. The message is addressed by its Message-ID header (from the client's
// listing); the :uid path param is accepted for symmetry with the other message
// routes but the training seam keys on the Message-ID the client supplies in the
// body (vulos-mail addresses messages by Message-ID, like the threads seam).
// POST /v1/messages/:uid/category  body {category, messageId}
func (h *Handler) handleRecategorize(c *fiber.Ctx) error {
	store, err := h.categoryStoreFor(c)
	if err != nil {
		return failCategories(c, err)
	}
	var body struct {
		Category  string `json:"category"`
		MessageID string `json:"messageId"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {category, messageId}")
	}
	cat, ok := normalizeCategory(body.Category)
	if !ok {
		return fail(c, fiber.StatusBadRequest, "invalid category")
	}
	mid := strings.TrimSpace(body.MessageID)
	if mid == "" {
		return fail(c, fiber.StatusBadRequest, "messageId is required")
	}
	trained, err := store.Recategorize(c.Context(), mid, cat)
	if err != nil {
		return failCategories(c, err)
	}
	return c.JSON(fiber.Map{"ok": true, "category": cat, "trained": trained})
}

// failCategories maps a category-store error to an HTTP response: unsupported →
// 501, anything else → 502.
func failCategories(c *fiber.Ctx, err error) error {
	if errors.Is(err, errCategoriesUnsupported) {
		return fail(c, fiber.StatusNotImplemented, err.Error())
	}
	return fail(c, fiber.StatusBadGateway, "category store unavailable")
}

// --- httpCategoryStore: brokered HTTP client to vulos-mail's category surface --

type httpCategoryStore struct {
	base    string // e.g. http://127.0.0.1:2080/internal/categories
	secret  string // shared broker secret → X-Vulos-Broker-Auth
	account string // brokered mailbox → account field (never client input)
	hc      *http.Client
}

func (s *httpCategoryStore) Resolve(ctx context.Context, messageIDs []string) (map[string]string, error) {
	if len(messageIDs) == 0 {
		return map[string]string{}, nil
	}
	if len(messageIDs) > maxResolveBatch {
		messageIDs = messageIDs[:maxResolveBatch]
	}
	b, err := json.Marshal(map[string]any{"account": s.account, "messageIds": messageIDs})
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
		Categories map[string]string `json:"categories"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	if out.Categories == nil {
		out.Categories = map[string]string{}
	}
	return out.Categories, nil
}

func (s *httpCategoryStore) Recategorize(ctx context.Context, messageID, category string) (bool, error) {
	b, err := json.Marshal(map[string]any{
		"account":   s.account,
		"messageId": messageID,
		"category":  category,
	})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/recategorize", bytes.NewReader(b))
	if err != nil {
		return false, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		OK      bool `json:"ok"`
		Trained bool `json:"trained"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return false, err
	}
	return out.Trained, nil
}

// doJSON performs the request and decodes a JSON response into out. The body is
// read through a LimitReader so a hostile/broken upstream cannot exhaust memory.
// A 501 upstream is mapped to errCategoriesUnsupported so the honest-degrade path
// propagates; any other non-2xx or transport error becomes a generic 502 error.
func (s *httpCategoryStore) doJSON(req *http.Request, out any) error {
	resp, err := s.hc.Do(req)
	if err != nil {
		return errors.New("category store unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxCategoryRespBytes))
	if resp.StatusCode == http.StatusNotImplemented {
		return errCategoriesUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("category store error")
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return errors.New("invalid category store response")
		}
	}
	return nil
}
