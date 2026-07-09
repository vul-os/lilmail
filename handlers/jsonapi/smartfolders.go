// handlers/jsonapi/smartfolders.go — /v1 SEMANTIC smart-folders + smart-card
// fields. A sibling of categories.go.
//
// SEAM: lilmail does not own classification. vulos-mail's on-box smart-folder
// classifier files each inbound message into at most one SEMANTIC smart-folder
// (bills/receipts/travel/shipping/subscriptions/statements) at ingest, extracts
// the schema.org structured card fields, and exposes both + a training signal via
// broker-gated internal HTTP endpoints. In a CP-brokered deployment vulos-mail
// injects the smart-folders base URL as X-Vulos-Mail-Smartfolders-Url; these
// handlers broker to it, presenting the shared broker secret as X-Vulos-Broker-
// Auth and the validated mailbox as the `account` field.
//
// SURFACE:
//   - GET /v1/messages?folder=<smart>  filters the inbox listing to one smart-
//     folder (augment-then-filter; see handleMessages). Also, each message on any
//     listing is stamped with its `smartFolder` (additive, like category).
//   - GET /v1/messages/:uid  additionally carries `smartFields` (the schema.org
//     card data) when the backend extracts them.
//   - POST /v1/messages/:uid/smartfolder  body {folder, messageId}  re-files the
//     message (folder="" clears it) AND trains the per-account model.
//
// HONEST DEGRADE: when no smart-folders URL is brokered (a plain Gmail/IMAP
// account, or standalone/session lilmail), the listing leaves each `smartFolder`
// empty (the client shows no smart-folders), `?folder=` is ignored, and the
// re-file endpoint reports 501. A non-hosted account is never errored.
//
// ISOLATION: `account` for every call is ALWAYS the validated broker mailbox
// (spec.Email) — never client input. A caller can only read/train/extract THEIR
// OWN mailbox. Training + model + extracted fields are per-account upstream.
//
// ROBUSTNESS: the target folder is validated against the fixed enum ("" allowed
// = clear) before it is sent upstream; the resolve batch + response bodies are
// bounded; classification runs on UNTRUSTED content upstream (bounded, no-ReDoS,
// XXE-safe, fail-safe to no-label); the smart-folder value is a plain enum token,
// and the smart-card fields are plain bounded strings — never rendered as raw
// untrusted markup here (the client escapes them).
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

// knownSmartFolders is the fixed set of smart-folders. Re-file input is validated
// against it (case-insensitive); the empty string is also accepted and means
// "clear the smart-folder label".
var knownSmartFolders = map[string]bool{
	"bills": true, "receipts": true, "travel": true,
	"shipping": true, "subscriptions": true, "statements": true,
}

// normalizeSmartFolder lowercases/trims a folder token and reports whether it is
// one of the known folders OR the empty string (clear). Returns ("", true) for a
// clear request so the caller can distinguish "clear" from "invalid".
func normalizeSmartFolder(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "none" {
		return "", true
	}
	if knownSmartFolders[s] {
		return s, true
	}
	return "", false
}

// smartFolderStore is the seam the smart-folder handlers use. The brokered HTTP
// client satisfies it in production; tests substitute a mock via newSmartFolderStore.
type smartFolderStore interface {
	// Resolve maps each of the given Message-IDs to its smart-folder. An un-filed
	// message is OMITTED (not reported as any folder). Unknown ids are omitted.
	Resolve(ctx context.Context, messageIDs []string) (map[string]string, error)
	// Refile files the message (by Message-ID header) into `folder` ("" clears it)
	// and trains the per-account model. Returns whether the training half succeeded.
	Refile(ctx context.Context, messageID, folder string) (bool, error)
	// Fields extracts the schema.org smart-card fields for a message. Returns
	// (nil, found=false) when no such message is held.
	Fields(ctx context.Context, messageID string) (*models.SmartFields, bool, error)
}

// errSmartFoldersUnsupported is surfaced as 501 when the backend does not do
// semantic smart-foldering (no X-Vulos-Mail-Smartfolders-Url brokered).
var errSmartFoldersUnsupported = errors.New("smart folders are not supported by this mailbox backend")

const maxSmartFolderRespBytes = 4 << 20 // 4 MiB

// newSmartFolderStore builds the brokered HTTP client. Package var so tests can
// substitute a mock without a live vulos-mail.
var newSmartFolderStore = func(baseURL, secret, account string) smartFolderStore {
	return &httpSmartFolderStore{
		base:    strings.TrimRight(baseURL, "/"),
		secret:  secret,
		account: account,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// smartFolderStoreFor resolves the store for a request. Smart-folders require the
// brokered path (the base URL arrives per request); a session-only or non-hosted
// request gets errSmartFoldersUnsupported.
func (h *Handler) smartFolderStoreFor(c *fiber.Ctx) (smartFolderStore, error) {
	spec, ok := brokerSpecOf(c)
	if !ok || strings.TrimSpace(spec.SmartFoldersURL) == "" {
		return nil, errSmartFoldersUnsupported
	}
	return newSmartFolderStore(spec.SmartFoldersURL, h.brokerSecret, spec.Email), nil
}

// registerSmartFolders mounts the /v1 smart-folder routes. Called from Register
// only when the broker path is active.
func (h *Handler) registerSmartFolders(g fiber.Router) {
	g.Post("/messages/:uid/smartfolder", h.handleRefile) // body {folder, messageId}
}

// attachSmartFolders resolves each message's smart-folder (by Message-ID header)
// and stamps emails[i].SmartFolder. Un-filed messages are left empty. A store
// error is swallowed (best-effort). Returns true only when the resolve SUCCEEDED
// and returned a non-empty result — the caller uses this to decide whether
// ?folder= filtering is safe. Mirrors attachCategories.
func (h *Handler) attachSmartFolders(ctx context.Context, store smartFolderStore, emails []models.Email) bool {
	ids := make([]string, 0, len(emails))
	for i := range emails {
		if mid := strings.TrimSpace(emails[i].MessageID); mid != "" {
			ids = append(ids, mid)
		}
	}
	if len(ids) == 0 {
		return false
	}
	folders, err := store.Resolve(ctx, ids)
	if err != nil || len(folders) == 0 {
		return false
	}
	norm := make(map[string]string, len(folders))
	for k, v := range folders {
		if fv, ok := normalizeSmartFolder(v); ok && fv != "" {
			norm[normalizeMessageID(k)] = fv
		}
	}
	for i := range emails {
		key := normalizeMessageID(emails[i].MessageID)
		if key == "" {
			continue
		}
		if fol, ok := norm[key]; ok {
			emails[i].SmartFolder = fol
		}
	}
	// Return true when the resolve itself succeeded (folders non-empty), even if the
	// current page happened to contain no filed messages — the resolve IS
	// authoritative (upstream reachable). This matches attachCategories' intent:
	// gate filtering on "upstream answered", not "this page had matches".
	return true
}

// filterBySmartFolder returns the subset of emails whose SmartFolder equals want
// (case-insensitive). Called only when ?folder= is a known smart-folder AND a
// store augmented the page. Filters the CURRENT PAGE only (pagination stays
// server-authoritative on the flat listing), like filterByCategory.
func filterBySmartFolder(emails []models.Email, want string) []models.Email {
	want = strings.ToLower(strings.TrimSpace(want))
	out := emails[:0]
	for _, e := range emails {
		if strings.EqualFold(e.SmartFolder, want) {
			out = append(out, e)
		}
	}
	return out
}

// attachSmartFields fetches the schema.org card fields for a SINGLE message and
// stamps email.SmartFields. Best-effort: a store error or miss leaves it nil (the
// client simply renders no card). Called on the single-message read path only.
func (h *Handler) attachSmartFields(ctx context.Context, store smartFolderStore, email *models.Email) {
	mid := strings.TrimSpace(email.MessageID)
	if mid == "" {
		return
	}
	fields, found, err := store.Fields(ctx, mid)
	if err != nil || !found || fields == nil {
		return
	}
	// Only attach when there is something to render (avoid an empty card object).
	if *fields != (models.SmartFields{}) {
		email.SmartFields = fields
	}
}

// handleRefile re-files a message to a smart-folder (or clears it) and trains the
// per-account model. Addressed by Message-ID header (from the client's listing).
// POST /v1/messages/:uid/smartfolder  body {folder, messageId}
func (h *Handler) handleRefile(c *fiber.Ctx) error {
	store, err := h.smartFolderStoreFor(c)
	if err != nil {
		return failSmartFolders(c, err)
	}
	var body struct {
		Folder    string `json:"folder"`
		MessageID string `json:"messageId"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {folder, messageId}")
	}
	folder, ok := normalizeSmartFolder(body.Folder)
	if !ok {
		return fail(c, fiber.StatusBadRequest, "invalid folder")
	}
	mid := strings.TrimSpace(body.MessageID)
	if mid == "" {
		return fail(c, fiber.StatusBadRequest, "messageId is required")
	}
	trained, err := store.Refile(c.Context(), mid, folder)
	if err != nil {
		return failSmartFolders(c, err)
	}
	return c.JSON(fiber.Map{"ok": true, "folder": folder, "trained": trained})
}

// failSmartFolders maps a store error to an HTTP response: unsupported → 501,
// anything else → 502.
func failSmartFolders(c *fiber.Ctx, err error) error {
	if errors.Is(err, errSmartFoldersUnsupported) {
		return fail(c, fiber.StatusNotImplemented, err.Error())
	}
	return fail(c, fiber.StatusBadGateway, "smart folder store unavailable")
}

// --- httpSmartFolderStore: brokered HTTP client to vulos-mail's surface ---

type httpSmartFolderStore struct {
	base    string // e.g. http://127.0.0.1:2080/internal/smartfolders
	secret  string
	account string // brokered mailbox → account field (never client input)
	hc      *http.Client
}

func (s *httpSmartFolderStore) Resolve(ctx context.Context, messageIDs []string) (map[string]string, error) {
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
		Folders map[string]string `json:"folders"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	if out.Folders == nil {
		out.Folders = map[string]string{}
	}
	return out.Folders, nil
}

func (s *httpSmartFolderStore) Refile(ctx context.Context, messageID, folder string) (bool, error) {
	b, err := json.Marshal(map[string]any{"account": s.account, "messageId": messageID, "folder": folder})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/refile", bytes.NewReader(b))
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

func (s *httpSmartFolderStore) Fields(ctx context.Context, messageID string) (*models.SmartFields, bool, error) {
	b, err := json.Marshal(map[string]any{"account": s.account, "messageId": messageID})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/fields", bytes.NewReader(b))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Found  bool                `json:"found"`
		Fields *models.SmartFields `json:"fields"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, false, err
	}
	return out.Fields, out.Found, nil
}

// doJSON performs the request and decodes a JSON response, bounded by a
// LimitReader. A 501 upstream maps to errSmartFoldersUnsupported; any other
// non-2xx becomes a generic 502-ish error.
func (s *httpSmartFolderStore) doJSON(req *http.Request, out any) error {
	resp, err := s.hc.Do(req)
	if err != nil {
		return errors.New("smart folder store unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxSmartFolderRespBytes))
	if resp.StatusCode == http.StatusNotImplemented {
		return errSmartFoldersUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("smart folder store error")
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return errors.New("invalid smart folder store response")
		}
	}
	return nil
}
