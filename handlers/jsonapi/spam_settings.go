// handlers/jsonapi/spam_settings.go — /v1/settings/spam : per-account spam-filter
// preferences (sensitivity / DNSBL opt-in / allow / block).
//
// SEAM: lilmail does NOT own spam state. The authoritative per-account spam prefs
// live in vulos-mail, where inbound DELIVERY runs and the scoring engine reads
// them. So — exactly like /v1/rules — these handlers broker CRUD to vulos-mail's
// broker-gated /internal/spam/settings endpoint, derived from the same
// X-Vulos-Mail-Rules-Url the rules/snooze/vacation surfaces use. When no rule-store
// URL is brokered (a plain Gmail/IMAP account, or standalone session lilmail),
// there is no vulos-mail spam engine to configure: the surface reports 501 and the
// mail-ui's SpamPanel shows its honest "not exposed by this server" state.
//
// The wire shape matches the SpamPanel client 1:1:
//
//	GET  /v1/settings/spam        → {sensitivity, dnsbl, allow[], block[]}
//	PUT  /v1/settings/spam {cfg}  → {sensitivity, dnsbl, allow[], block[]}
//
// SECURITY: per-OWNER isolation — the brokered account is the authenticated
// identity (spec.Email / fromEmail), sent to vulos-mail as X-Vulos-Spam-Account
// so one owner can never read or set another's prefs. Input is validated + bounded
// HERE (sensitivity enum, list-size cap, each entry a clean address/domain with no
// CR/LF/NUL/injection) before it ever leaves the process, independently of the
// authoritative validation vulos-mail also runs. Fail-safe: a rejected entry is a
// 400 and nothing is sent upstream.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// spamConfig is the client-facing spam preferences shape.
type spamConfig struct {
	Sensitivity string   `json:"sensitivity"`
	DNSBL       bool     `json:"dnsbl"`
	Allow       []string `json:"allow"`
	Block       []string `json:"block"`
}

// maxSpamListEntries bounds each of the allow/block lists (mirrors vulos-mail).
const maxSpamListEntries = 500

// spamSettingsURLFromRules derives vulos-mail's /internal/spam/settings endpoint
// from the brokered rule-store URL (…/internal/mailrules → …/internal/spam/settings),
// the same derivation the vacation/snooze pushes use. Returns "" when no rule-store
// URL is brokered — i.e. the mailbox is not vulos-mail-hosted, so there is no spam
// engine to configure.
func spamSettingsURLFromRules(rulesURL string) string {
	rulesURL = strings.TrimRight(strings.TrimSpace(rulesURL), "/")
	if rulesURL == "" {
		return ""
	}
	if strings.HasSuffix(rulesURL, "/internal/mailrules") {
		return strings.TrimSuffix(rulesURL, "/mailrules") + "/spam/settings"
	}
	// Unknown shape: best-effort sibling under the same parent path.
	if i := strings.LastIndex(rulesURL, "/"); i > 0 {
		return rulesURL[:i] + "/spam/settings"
	}
	return ""
}

// spamStoreFor resolves the brokered spam-settings endpoint + owner for a request,
// or reports that this mailbox has no vulos-mail spam engine (→ 501). owner is the
// authenticated brokered identity, which is the authz boundary.
func (h *Handler) spamStoreFor(c *fiber.Ctx) (url, owner string, ok bool) {
	spec, isBroker := brokerSpecOf(c)
	if !isBroker {
		return "", "", false
	}
	u := spamSettingsURLFromRules(spec.RulesURL)
	if u == "" {
		return "", "", false
	}
	return u, spec.Email, true
}

func (h *Handler) handleGetSpam(c *fiber.Ctx) error {
	url, owner, ok := h.spamStoreFor(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "spam settings are not supported by this mailbox backend")
	}
	cfg, err := brokerSpamGet(c.Context(), url, h.brokerSecret, owner)
	if err != nil {
		return failSpam(c, err)
	}
	return c.JSON(spamPublic(cfg))
}

func (h *Handler) handlePutSpam(c *fiber.Ctx) error {
	url, owner, ok := h.spamStoreFor(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "spam settings are not supported by this mailbox backend")
	}
	var in spamConfig
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	// Validate + bound locally BEFORE brokering (defence in depth; vulos-mail
	// validates again authoritatively).
	norm, verr := validateSpamConfig(in)
	if verr != "" {
		return fail(c, fiber.StatusBadRequest, verr)
	}
	stored, err := brokerSpamPut(c.Context(), url, h.brokerSecret, owner, norm)
	if err != nil {
		return failSpam(c, err)
	}
	return c.JSON(spamPublic(stored))
}

// validateSpamConfig enforces the sensitivity enum, list bounds, and per-entry
// address/domain shape (no CR/LF/NUL/whitespace injection). Returns a non-empty
// error string on rejection (fail-safe: caller returns 400 and sends nothing).
func validateSpamConfig(in spamConfig) (spamConfig, string) {
	sens := strings.ToLower(strings.TrimSpace(in.Sensitivity))
	switch sens {
	case "", "standard":
		sens = "standard"
	case "low", "high":
	default:
		return spamConfig{}, "sensitivity must be one of: low, standard, high"
	}
	allow, err := validateSpamEntries(in.Allow)
	if err != "" {
		return spamConfig{}, "allow list: " + err
	}
	block, err := validateSpamEntries(in.Block)
	if err != "" {
		return spamConfig{}, "block list: " + err
	}
	return spamConfig{Sensitivity: sens, DNSBL: in.DNSBL, Allow: allow, Block: block}, ""
}

// validateSpamEntries lowercases, trims, validates, dedups and caps the list.
func validateSpamEntries(in []string) ([]string, string) {
	if len(in) > maxSpamListEntries {
		return nil, "too many entries (max 500)"
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !validSpamEntry(e) {
			return nil, "invalid entry (must be an address or domain): " + e
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, ""
}

// validSpamEntry reports whether e is a safe allow/block entry: a bare domain
// ("example.com") or a full address ("a@example.com"), with no control /
// whitespace / injection characters and a bounded length.
func validSpamEntry(e string) bool {
	if e == "" || len(e) > 254 {
		return false
	}
	at := 0
	for _, r := range e {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '+':
		case r == '@':
			at++
		default:
			return false
		}
	}
	if at > 1 {
		return false
	}
	domain := e
	if at == 1 {
		i := strings.IndexByte(e, '@')
		local, d := e[:i], e[i+1:]
		if local == "" {
			return false
		}
		domain = d
	}
	return validSpamDomain(domain)
}

// validSpamDomain reports whether d is a plausible DNS domain (bounded, labelled,
// at least one dot).
func validSpamDomain(d string) bool {
	if d == "" || len(d) > 253 || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
	}
	return true
}

// spamPublic returns the client wire shape with non-nil slices (JSON [] not null)
// and a defaulted sensitivity.
func spamPublic(cfg spamConfig) fiber.Map {
	sens := cfg.Sensitivity
	if sens == "" {
		sens = "standard"
	}
	allow := cfg.Allow
	if allow == nil {
		allow = []string{}
	}
	block := cfg.Block
	if block == nil {
		block = []string{}
	}
	return fiber.Map{"sensitivity": sens, "dnsbl": cfg.DNSBL, "allow": allow, "block": block}
}

// --- brokered HTTP client to vulos-mail's /internal/spam/settings ------------

// spamAPIError carries an upstream status + message so the handler can propagate a
// meaningful code (e.g. 400 validation, 501 not configured) rather than 502.
type spamAPIError struct {
	status int
	msg    string
}

func (e *spamAPIError) Error() string { return e.msg }

// brokerSpamGet fetches the account's stored prefs from vulos-mail. Package var
// for the test seam (mirrors pushVacationConfig / postSnoozeSchedule).
var brokerSpamGet = func(ctx context.Context, storeURL, secret, account string) (spamConfig, error) {
	u := storeURL + "?account=" + url.QueryEscape(account)
	var out spamConfig
	if err := spamBrokerDo(ctx, http.MethodGet, u, secret, account, nil, &out); err != nil {
		return spamConfig{}, err
	}
	return out, nil
}

// brokerSpamPut stores the account's prefs at vulos-mail and returns the echoed
// stored config. Package var for the test seam.
var brokerSpamPut = func(ctx context.Context, storeURL, secret, account string, cfg spamConfig) (spamConfig, error) {
	body := map[string]any{
		"account":     account,
		"sensitivity": cfg.Sensitivity,
		"dnsbl":       cfg.DNSBL,
		"allow":       cfg.Allow,
		"block":       cfg.Block,
	}
	var out spamConfig
	if err := spamBrokerDo(ctx, http.MethodPut, storeURL, secret, account, body, &out); err != nil {
		return spamConfig{}, err
	}
	return out, nil
}

// spamBrokerDo performs one brokered request against vulos-mail's spam-settings
// endpoint, presenting the shared broker secret. The account is sent in the body /
// query (vulos-mail keys strictly on it), enforcing per-owner isolation upstream.
func spamBrokerDo(ctx context.Context, method, url, secret, account string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set(hdrBrokerAuth, secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return &spamAPIError{status: http.StatusBadGateway, msg: "spam settings backend unreachable"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := ""
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil {
			msg = e.Error
		}
		return &spamAPIError{status: resp.StatusCode, msg: msg}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &spamAPIError{status: http.StatusBadGateway, msg: "invalid spam settings response"}
		}
	}
	return nil
}

// failSpam maps a broker error to an HTTP response: upstream spamAPIError → its
// status (so a 501 "not configured" or 400 validation propagates), anything else
// → 502.
func failSpam(c *fiber.Ctx, err error) error {
	var apiErr *spamAPIError
	if errors.As(err, &apiErr) {
		msg := apiErr.msg
		if msg == "" {
			msg = "spam settings error"
		}
		return fail(c, apiErr.status, msg)
	}
	return fail(c, fiber.StatusBadGateway, "spam settings backend unavailable")
}
