// handlers/jsonapi/settings.go — /v1/settings/* : vacation responder, signatures,
// and send-as identities. All three are per-account (keyed by fromEmail), authed
// like the rest of jsonapi (session OR broker), and durable-KV backed (501 when no
// KV was wired). User-authored HTML (signature/vacation body) is sanitized with
// sanitizeSnippetHTML; header-bearing fields (vacation Subject, identity address)
// are guarded against CR/LF/NUL injection via the wave-49 validateHeaderValue.
package jsonapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// registerSettings mounts /v1/settings/*.
func (h *Handler) registerSettings(g fiber.Router) {
	g.Get("/settings/vacation", h.handleGetVacation)
	g.Put("/settings/vacation", h.handlePutVacation)
	g.Get("/settings/signatures", h.handleGetSignatures)
	g.Put("/settings/signatures", h.handlePutSignatures)
	g.Get("/settings/identities", h.handleGetIdentities)
	g.Put("/settings/identities", h.handlePutIdentities)
	g.Get("/settings/spam", h.handleGetSpam)
	g.Put("/settings/spam", h.handlePutSpam)
}

// settingsStoreOr501 resolves the settings store + the authenticated owner. When
// it returns handled==true it has ALREADY written the error response (501 no KV,
// 401 no identity) and the caller must return herr immediately. (fail returns the
// nil that a fiber handler returns on success, so it is threaded back as herr.)
func (h *Handler) settingsStoreOr501(c *fiber.Ctx) (store *settingsStore, owner string, handled bool, herr error) {
	if h.kv == nil {
		return nil, "", true, fail(c, fiber.StatusNotImplemented, "settings storage is not enabled")
	}
	owner = h.fromEmail(c)
	if strings.TrimSpace(owner) == "" {
		return nil, "", true, fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	return newSettingsStore(h.kv), owner, false, nil
}

// --- Vacation / out-of-office responder --------------------------------------

// handleGetVacation returns the account's vacation responder config.
//
// ENFORCEMENT NOTE (broker model): lilmail brokers to the upstream provider's
// IMAP; it does NOT run the inbound delivery path (that is the provider, or
// vulos-mail where it does). So storing "enabled" here does not, by itself, make
// the provider auto-reply. This surface is the authoritative CONFIG the client
// edits; actual enforcement happens where delivery runs:
//   - standalone lilmail with a local delivery/rules path applies it on inbound;
//   - a brokered account whose backend exposes a rule/Sieve store (the same
//     X-Vulos-Mail-Rules-Url the /v1/rules surface uses) can push the vacation
//     as a Sieve "vacation" action — a follow-up wiring, not this endpoint's job;
//   - a plain Gmail/IMAP brokered account: the config is stored + exposed, and
//     enforcement must be set on the provider's own vacation setting.
//
// The response therefore always echoes the stored config so the UI is truthful
// about what WILL apply once the delivery path honours it.
func (h *Handler) handleGetVacation(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var cfg vacationConfig
	if err := store.get(owner, kindVacation, &cfg); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not load vacation settings")
	}
	return c.JSON(vacationPublic(cfg))
}

// handlePutVacation validates + stores the vacation responder config. The Subject
// is header-injection-guarded (it becomes a real mail Subject on auto-reply); the
// Body is sanitized (it becomes an outgoing HTML part). Dates, when present, must
// be RFC3339 and startAt must not be after endAt.
func (h *Handler) handlePutVacation(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var in vacationConfig
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}

	in.Subject = strings.TrimSpace(in.Subject)
	// The subject is emitted as a mail header on every auto-reply: reject CR/LF/NUL.
	if err := api.ValidateHeaderValue(in.Subject); err != nil {
		return fail(c, fiber.StatusBadRequest, "subject contains illegal characters")
	}
	if in.Enabled && in.Subject == "" {
		return fail(c, fiber.StatusBadRequest, "an enabled responder needs a subject")
	}
	// The body is emitted as an outgoing HTML part: sanitize active content out.
	in.Body = sanitizeSnippetHTML(in.Body)

	// Optional scheduling window: parse + order-check.
	start, err := parseOptRFC3339(in.StartAt)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "startAt must be RFC3339")
	}
	end, err := parseOptRFC3339(in.EndAt)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "endAt must be RFC3339")
	}
	if start != nil && end != nil && end.Before(*start) {
		return fail(c, fiber.StatusBadRequest, "endAt must not be before startAt")
	}

	if err := store.put(owner, kindVacation, &in); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not save vacation settings")
	}

	// Push the config to the firing engine for vulos-mail-hosted mailboxes. The KV
	// above is the UI's read model; vulos-mail is where inbound DELIVERY runs, so it
	// is the only place a server-side auto-reply can actually fire. We derive its
	// broker-gated /internal/vacation endpoint from the same rule-store URL the /v1
	// rules + snooze surfaces use (present only for a vulos-mail-hosted account). For
	// an externally-brokered IMAP account (Gmail/Outlook/plain IMAP) there is no such
	// endpoint: the config is stored + exposed here, but the external provider runs
	// its own vacation responder — we do not pretend to enforce it. Best-effort: a
	// push failure does not fail the save (the config is durably stored either way).
	serverEnforced := false
	if spec, ok := brokerSpecOf(c); ok {
		if storeURL := vacationStoreURLFromRules(spec.RulesURL); storeURL != "" {
			if err := pushVacationConfig(c.Context(), storeURL, h.brokerSecret, spec.Email, in); err == nil {
				serverEnforced = true
			}
		}
	}
	out := vacationPublic(in)
	out["serverEnforced"] = serverEnforced
	return c.JSON(out)
}

// vacationStoreURLFromRules derives vulos-mail's /internal/vacation endpoint from
// the brokered rule-store URL (…/internal/mailrules → …/internal/vacation), the
// same derivation the snooze schedule uses. Returns "" when no rule-store URL is
// brokered — i.e. the mailbox is not vulos-mail-hosted, so there is no firing
// engine to push to.
func vacationStoreURLFromRules(rulesURL string) string {
	rulesURL = strings.TrimRight(strings.TrimSpace(rulesURL), "/")
	if rulesURL == "" {
		return ""
	}
	if strings.HasSuffix(rulesURL, "/internal/mailrules") {
		return strings.TrimSuffix(rulesURL, "/mailrules") + "/vacation"
	}
	// Unknown shape: best-effort sibling under the same parent path.
	if i := strings.LastIndex(rulesURL, "/"); i > 0 {
		return rulesURL[:i] + "/vacation"
	}
	return ""
}

// pushVacationConfig PUTs the vacation config to vulos-mail's broker-gated
// /internal/vacation endpoint so the delivery-time responder honours it. Package
// var for the test seam (mirrors postSnoozeSchedule).
var pushVacationConfig = func(ctx context.Context, storeURL, secret, account string, cfg vacationConfig) error {
	body := map[string]any{
		"account": account,
		"enabled": cfg.Enabled,
		"subject": cfg.Subject,
		"body":    cfg.Body,
		"startAt": cfg.StartAt,
		"endAt":   cfg.EndAt,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, storeURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vulos-Broker-Auth", secret)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &fiber.Error{Code: resp.StatusCode, Message: "vacation config rejected by engine"}
	}
	return nil
}

// vacationActive reports whether the responder should fire for a message received
// at `now`, given the config's enabled flag + optional start/end window. It is the
// single source of truth the delivery path (or a test) uses so the "scheduling"
// contract is enforced in one place. Loop/backscatter protection is a SEPARATE
// gate (shouldAutoReply) applied to the triggering message's headers.
func vacationActive(cfg vacationConfig, now time.Time) bool {
	if !cfg.Enabled {
		return false
	}
	if s, err := parseOptRFC3339(cfg.StartAt); err == nil && s != nil && now.Before(*s) {
		return false
	}
	if e, err := parseOptRFC3339(cfg.EndAt); err == nil && e != nil && now.After(*e) {
		return false
	}
	return true
}

// shouldAutoReply is the loop/backscatter guard: an out-of-office reply must NEVER
// be sent to another auto-reply, to list traffic, or to a null/bounce sender —
// that is how mail loops and backscatter storms form. It inspects the triggering
// message's headers and returns false when any auto-submission / list marker is
// present, or the envelope sender is empty. Applied by the delivery path before
// composing a reply. Header names are matched case-insensitively.
func shouldAutoReply(fromEnvelope string, headers map[string]string) bool {
	if strings.TrimSpace(fromEnvelope) == "" {
		return false // null sender (a bounce / DSN) — never reply
	}
	lower := make(map[string]string, len(headers))
	for k, v := range headers {
		lower[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(v)
	}
	// RFC 3834 / common auto-response markers.
	if v := lower["auto-submitted"]; v != "" && v != "no" {
		return false
	}
	if lower["x-auto-response-suppress"] != "" {
		return false
	}
	if lower["precedence"] == "bulk" || lower["precedence"] == "list" || lower["precedence"] == "junk" {
		return false
	}
	// Any List-* header => mailing list; do not reply.
	for k := range lower {
		if strings.HasPrefix(k, "list-") {
			return false
		}
	}
	if lower["x-mailer"] == "" && lower["x-loop"] != "" {
		return false
	}
	return true
}

func vacationPublic(cfg vacationConfig) fiber.Map {
	return fiber.Map{
		"enabled":               cfg.Enabled,
		"subject":               cfg.Subject,
		"body":                  cfg.Body,
		"startAt":               cfg.StartAt,
		"endAt":                 cfg.EndAt,
		"respondOnlyToContacts": cfg.RespondOnlyToContacts,
	}
}

// --- Signatures --------------------------------------------------------------

// maxSignatures bounds how many named signatures an account may store.
const maxSignatures = 30

// handleGetSignatures returns the account's named signatures.
func (h *Handler) handleGetSignatures(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var sigs []signature
	if err := store.get(owner, kindSignatures, &sigs); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not load signatures")
	}
	if sigs == nil {
		sigs = []signature{}
	}
	return c.JSON(fiber.Map{"signatures": sigs})
}

// handlePutSignatures replaces the account's signature set. Each signature's HTML
// is sanitized; the id is server-assigned when absent so a client can create by
// omitting it. At most one signature may be the default.
func (h *Handler) handlePutSignatures(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var body struct {
		Signatures []signature `json:"signatures"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {signatures:[...]}")
	}
	if len(body.Signatures) > maxSignatures {
		return fail(c, fiber.StatusBadRequest, "too many signatures")
	}
	seenDefault := false
	out := make([]signature, 0, len(body.Signatures))
	for _, s := range body.Signatures {
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			return fail(c, fiber.StatusBadRequest, "each signature needs a name")
		}
		s.HTML = sanitizeSnippetHTML(s.HTML)
		if s.ID == "" {
			s.ID = newSettingsID()
		}
		if s.Default {
			if seenDefault {
				return fail(c, fiber.StatusBadRequest, "only one signature may be the default")
			}
			seenDefault = true
		}
		out = append(out, s)
	}
	if err := store.put(owner, kindSignatures, out); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not save signatures")
	}
	return c.JSON(fiber.Map{"signatures": out})
}

// --- Send-as identities ------------------------------------------------------

// handleGetIdentities returns the account's send-as identities. The primary
// mailbox (fromEmail) is ALWAYS included first as IsPrimary, whether or not any
// aliases were stored, so the compose From selector always has a valid default.
// A per-identity default signature id is echoed so the client can pair them.
func (h *Handler) handleGetIdentities(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var stored []identity
	if err := store.get(owner, kindIdentities, &stored); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not load identities")
	}
	// Always lead with the primary mailbox; never let a stored alias masquerade as
	// or shadow the real account address.
	out := []identity{{Address: owner, IsPrimary: true}}
	for _, id := range stored {
		id.Address = strings.TrimSpace(id.Address)
		if id.Address == "" || strings.EqualFold(id.Address, owner) {
			continue // skip blanks + any duplicate of the primary
		}
		id.IsPrimary = false
		out = append(out, id)
	}
	return c.JSON(fiber.Map{"identities": out})
}

// maxIdentities bounds how many send-as aliases an account may store. Mirrors
// vulos-mail's mailsettings.MaxAliases, which is the authoritative bound.
const maxIdentities = 20

// handlePutIdentities replaces the account's send-as identities (aliases). It is
// the write half of the identities surface, mirroring handlePutSignatures /
// handlePutVacation: same auth (session OR broker, scoped to fromEmail), same KV,
// same "PUT replaces the whole set" contract.
//
// AUTHORITY — an alias is a security decision, not a preference: sending as an
// address means claiming it. lilmail is NOT the authority for that claim. So for a
// vulos-mail-hosted mailbox the alias list is PUSHED to the engine's broker-gated
// /internal/identities FIRST, and the engine authorizes each alias against the
// verified-domain rule (an address at a domain the tenant has proven it owns, or a
// plus-subaddress of the account's own mailbox). If the engine REFUSES (403) or is
// unreachable, we store NOTHING and propagate the refusal — fail-closed, so the
// UI can never offer a From the mail server will reject at submission (and no one
// can register ceo@google.com by writing straight to this surface).
//
// For an externally-brokered mailbox (Gmail/Outlook/plain IMAP) there is no
// vulos-mail engine to authorize against: the identities are stored as the client's
// read model, `serverEnforced` is false, and the upstream provider's SMTP server
// remains the authority for what From it will accept. We do not pretend otherwise.
//
// The primary mailbox is implicit — it is always returned by GET and is never
// stored as an alias, so a client cannot remove, rename, or shadow it.
//
// SCOPE: send-as ONLY. Registering an alias does NOT make it a delivery address —
// mail sent TO an alias is not accepted (inbound alias/group delivery is a separate,
// unbuilt feature).
func (h *Handler) handlePutIdentities(c *fiber.Ctx) error {
	store, owner, handled, herr := h.settingsStoreOr501(c)
	if handled {
		return herr
	}
	var body struct {
		Identities []identity `json:"identities"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {identities:[...]}")
	}
	if len(body.Identities) > maxIdentities+1 { // +1: a client may echo the primary back
		return fail(c, fiber.StatusBadRequest, "too many identities")
	}

	seen := make(map[string]struct{}, len(body.Identities))
	out := make([]identity, 0, len(body.Identities))
	aliases := make([]string, 0, len(body.Identities))
	for _, id := range body.Identities {
		addr := strings.ToLower(strings.TrimSpace(id.Address))
		if addr == "" || strings.EqualFold(addr, owner) {
			continue // blanks, and the implicit primary, are never stored as aliases
		}
		// The address becomes a real From header: reject CR/LF/NUL, then the shape.
		if err := api.ValidateHeaderValue(addr); err != nil || !validIdentityAddress(addr) {
			return fail(c, fiber.StatusBadRequest, "invalid identity address: "+addr)
		}
		id.Name = strings.TrimSpace(id.Name)
		if err := api.ValidateHeaderValue(id.Name); err != nil {
			return fail(c, fiber.StatusBadRequest, "identity name contains illegal characters")
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		id.Address, id.IsPrimary = addr, false
		out = append(out, id)
		aliases = append(aliases, addr)
	}
	if len(out) > maxIdentities {
		return fail(c, fiber.StatusBadRequest, "too many identities")
	}

	// Register with the authority BEFORE storing (fail-closed).
	serverEnforced := false
	if spec, ok := brokerSpecOf(c); ok {
		if storeURL := identitiesStoreURLFromRules(spec.RulesURL); storeURL != "" {
			if err := pushIdentities(c.Context(), storeURL, h.brokerSecret, spec.Email, aliases); err != nil {
				var fe *fiber.Error
				if errors.As(err, &fe) && (fe.Code == fiber.StatusForbidden ||
					fe.Code == fiber.StatusBadRequest || fe.Code == fiber.StatusNotImplemented) {
					msg := fe.Message
					if msg == "" {
						msg = "the mail server refused these send-as identities"
					}
					return fail(c, fe.Code, msg)
				}
				return fail(c, fiber.StatusBadGateway, "could not register send-as identities with the mail server")
			}
			serverEnforced = true
		}
	}

	if err := store.put(owner, kindIdentities, out); err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not save identities")
	}
	// Echo the same shape GET returns (primary first, never removable).
	return c.JSON(fiber.Map{
		"identities":     append([]identity{{Address: owner, IsPrimary: true}}, out...),
		"serverEnforced": serverEnforced,
	})
}

// identitiesStoreURLFromRules derives vulos-mail's /internal/identities endpoint
// from the brokered rule-store URL (…/internal/mailrules → …/internal/identities),
// the same derivation the vacation/spam/snooze pushes use. Returns "" when no
// rule-store URL is brokered — i.e. the mailbox is not vulos-mail-hosted, so there
// is no send-as authority to register with.
func identitiesStoreURLFromRules(rulesURL string) string {
	rulesURL = strings.TrimRight(strings.TrimSpace(rulesURL), "/")
	if rulesURL == "" {
		return ""
	}
	if strings.HasSuffix(rulesURL, "/internal/mailrules") {
		return strings.TrimSuffix(rulesURL, "/mailrules") + "/identities"
	}
	// Unknown shape: best-effort sibling under the same parent path.
	if i := strings.LastIndex(rulesURL, "/"); i > 0 {
		return rulesURL[:i] + "/identities"
	}
	return ""
}

// pushIdentities registers the account's alias addresses with vulos-mail's
// broker-gated /internal/identities, where they are AUTHORIZED and persisted into
// the store the send path checks. A non-2xx is returned as a *fiber.Error carrying
// the upstream status + message so the caller can propagate a 403 ("not an address
// this account may send as") rather than a generic failure. Package var for the
// test seam (mirrors pushVacationConfig).
var pushIdentities = func(ctx context.Context, storeURL, secret, account string, aliases []string) error {
	if aliases == nil {
		aliases = []string{}
	}
	b, _ := json.Marshal(map[string]any{"account": account, "aliases": aliases})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, storeURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vulos-Broker-Auth", secret)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &fiber.Error{Code: resp.StatusCode, Message: e.Error}
	}
	return nil
}

// validIdentityAddress reports whether a is a safe send-as address:
// "<local>@<domain>" with exactly one '@', a label-shaped domain, and no control /
// whitespace / injection character. It is the LOCAL (defence-in-depth) half of the
// check — vulos-mail re-validates authoritatively AND decides whether the account
// may actually claim the address (this function does not, and cannot, know that).
func validIdentityAddress(a string) bool {
	if a == "" || len(a) > 254 {
		return false
	}
	at := 0
	for _, r := range a {
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
	if at != 1 {
		return false
	}
	i := strings.IndexByte(a, '@')
	if a[:i] == "" {
		return false
	}
	return validSpamDomain(a[i+1:])
}

// --- helpers -----------------------------------------------------------------

// newSettingsID returns a short opaque id for a signature.
func newSettingsID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseOptRFC3339 parses an optional RFC3339 timestamp: "" => (nil, nil); a bad
// value => error; a good value => the parsed time.
func parseOptRFC3339(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
