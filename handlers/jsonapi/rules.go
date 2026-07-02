// handlers/jsonapi/rules.go — /v1/rules CRUD for per-account inbound filters.
//
// SEAM: lilmail does not own rule state. The authoritative per-account rule store
// lives in vulos-mail (where inbound delivery runs, so rules can actually fire on
// new mail). In a CP-brokered deployment vulos-mail injects the rule-store base
// URL as X-Vulos-Mail-Rules-Url; these handlers broker CRUD to it over HTTP,
// presenting the shared broker secret as X-Vulos-Rules-Auth and the validated
// mailbox as X-Vulos-Rules-Account. When no rule-store URL is brokered (e.g. a
// plain Gmail/IMAP account, or standalone session lilmail), the surface reports
// 501 "not supported by this mailbox backend" and the mail-ui hides Filters.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// ruleStore is the seam the rule handlers use. The brokered HTTP client
// (httpRuleStore) satisfies it in production; tests substitute a mock via
// newRuleStore.
type ruleStore interface {
	List(ctx context.Context) ([]models.MailRule, error)
	Create(ctx context.Context, r models.MailRule) (models.MailRule, error)
	Update(ctx context.Context, id string, r models.MailRule) (models.MailRule, error)
	Delete(ctx context.Context, id string) error
	Reorder(ctx context.Context, order []string) ([]models.MailRule, error)
	Run(ctx context.Context, folder string, limit int) (matched, applied int, err error)
}

// errRulesUnsupported is surfaced as 501 when the mailbox backend has no rule
// store (no X-Vulos-Mail-Rules-Url brokered for this request).
var errRulesUnsupported = errors.New("rules are not supported by this mailbox backend")

// ruleAPIError carries an upstream (vulos-mail rule store) status + message so the
// handler can propagate a meaningful code (e.g. 400 validation, 404 not found)
// rather than flattening everything to 502.
type ruleAPIError struct {
	status int
	msg    string
}

func (e *ruleAPIError) Error() string { return e.msg }

// newRuleStore builds the brokered HTTP rule store client. Package var so tests
// can substitute a mock without a live vulos-mail.
var newRuleStore = func(baseURL, secret, account string) ruleStore {
	return &httpRuleStore{
		base:    strings.TrimRight(baseURL, "/"),
		secret:  secret,
		account: account,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// ruleStoreFor resolves the rule store for a request. Rules require the brokered
// path (the rule-store URL arrives per request); a session-only request has no
// rule store and gets errRulesUnsupported → 501.
func (h *Handler) ruleStoreFor(c *fiber.Ctx) (ruleStore, error) {
	spec, ok := brokerSpecOf(c)
	if !ok || strings.TrimSpace(spec.RulesURL) == "" {
		return nil, errRulesUnsupported
	}
	return newRuleStore(spec.RulesURL, h.brokerSecret, spec.Email), nil
}

// registerRules mounts the /v1/rules routes on the group. Called from Register
// when the broker path is active.
func (h *Handler) registerRules(g fiber.Router) {
	g.Get("/rules", h.handleListRules)
	g.Post("/rules", h.handleCreateRule)
	g.Post("/rules/reorder", h.handleReorderRules)
	g.Post("/rules/run", h.handleRunRules)
	g.Put("/rules/:id", h.handleUpdateRule)
	g.Delete("/rules/:id", h.handleDeleteRule)
}

func (h *Handler) handleListRules(c *fiber.Ctx) error {
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	rules, err := store.List(c.Context())
	if err != nil {
		return failRules(c, err)
	}
	return c.JSON(fiber.Map{"rules": nonNilRules(rules)})
}

func (h *Handler) handleCreateRule(c *fiber.Ctx) error {
	var r models.MailRule
	if err := c.BodyParser(&r); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid rule JSON")
	}
	if err := validateRuleShape(r); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	r.ID = "" // server assigns
	created, err := store.Create(c.Context(), r)
	if err != nil {
		return failRules(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"rule": created})
}

func (h *Handler) handleUpdateRule(c *fiber.Ctx) error {
	id := c.Params("id")
	var r models.MailRule
	if err := c.BodyParser(&r); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid rule JSON")
	}
	if err := validateRuleShape(r); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	updated, err := store.Update(c.Context(), id, r)
	if err != nil {
		return failRules(c, err)
	}
	return c.JSON(fiber.Map{"rule": updated})
}

func (h *Handler) handleDeleteRule(c *fiber.Ctx) error {
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	if err := store.Delete(c.Context(), c.Params("id")); err != nil {
		return failRules(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) handleReorderRules(c *fiber.Ctx) error {
	var body struct {
		Order []string `json:"order"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {order:[id,...]}")
	}
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	rules, err := store.Reorder(c.Context(), body.Order)
	if err != nil {
		return failRules(c, err)
	}
	return c.JSON(fiber.Map{"rules": nonNilRules(rules)})
}

func (h *Handler) handleRunRules(c *fiber.Ctx) error {
	var body struct {
		Folder string `json:"folder"`
		Limit  int    `json:"limit"`
	}
	_ = c.BodyParser(&body) // empty body is fine — defaults apply
	store, err := h.ruleStoreFor(c)
	if err != nil {
		return failRules(c, err)
	}
	matched, applied, err := store.Run(c.Context(), body.Folder, body.Limit)
	if err != nil {
		return failRules(c, err)
	}
	return c.JSON(fiber.Map{"matched": matched, "applied": applied})
}

// validateRuleShape does a light client-side sanity + safety check before we ever
// hit the network: it rejects footgun actions (auto-forward / permanent delete)
// with a clear 400, independent of the authoritative validation vulos-mail runs.
func validateRuleShape(r models.MailRule) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("rule name is required")
	}
	if len(r.Conditions) == 0 {
		return errors.New("rule needs at least one condition")
	}
	if len(r.Actions) == 0 {
		return errors.New("rule needs at least one action")
	}
	for _, a := range r.Actions {
		if models.ForbiddenRuleActions[strings.ToLower(strings.TrimSpace(a.Type))] {
			return fmt.Errorf("action %q is not allowed (auto-forward and permanent delete are disabled; use trash for recoverable removal)", a.Type)
		}
	}
	return nil
}

// failRules maps a rule-store error to an HTTP response: unsupported → 501,
// upstream ruleAPIError → its status, anything else → 502.
func failRules(c *fiber.Ctx, err error) error {
	if errors.Is(err, errRulesUnsupported) {
		return fail(c, fiber.StatusNotImplemented, err.Error())
	}
	var apiErr *ruleAPIError
	if errors.As(err, &apiErr) {
		msg := apiErr.msg
		if msg == "" {
			msg = "rule store error"
		}
		return fail(c, apiErr.status, msg)
	}
	return fail(c, fiber.StatusBadGateway, "rule store unavailable")
}

func nonNilRules(r []models.MailRule) []models.MailRule {
	if r == nil {
		return []models.MailRule{}
	}
	return r
}

// --- httpRuleStore: brokered HTTP client to vulos-mail's rule store -----------

const (
	hdrRulesAuth    = "X-Vulos-Rules-Auth"
	hdrRulesAccount = "X-Vulos-Rules-Account"
)

type httpRuleStore struct {
	base    string // e.g. http://127.0.0.1:2080/internal/mailrules
	secret  string // shared broker secret → X-Vulos-Rules-Auth
	account string // brokered mailbox → X-Vulos-Rules-Account
	hc      *http.Client
}

func (s *httpRuleStore) List(ctx context.Context) ([]models.MailRule, error) {
	var out struct {
		Rules []models.MailRule `json:"rules"`
	}
	if err := s.do(ctx, http.MethodGet, "", nil, &out); err != nil {
		return nil, err
	}
	return out.Rules, nil
}

func (s *httpRuleStore) Create(ctx context.Context, r models.MailRule) (models.MailRule, error) {
	var out struct {
		Rule models.MailRule `json:"rule"`
	}
	if err := s.do(ctx, http.MethodPost, "", r, &out); err != nil {
		return models.MailRule{}, err
	}
	return out.Rule, nil
}

func (s *httpRuleStore) Update(ctx context.Context, id string, r models.MailRule) (models.MailRule, error) {
	var out struct {
		Rule models.MailRule `json:"rule"`
	}
	if err := s.do(ctx, http.MethodPut, "/"+id, r, &out); err != nil {
		return models.MailRule{}, err
	}
	return out.Rule, nil
}

func (s *httpRuleStore) Delete(ctx context.Context, id string) error {
	return s.do(ctx, http.MethodDelete, "/"+id, nil, nil)
}

func (s *httpRuleStore) Reorder(ctx context.Context, order []string) ([]models.MailRule, error) {
	var out struct {
		Rules []models.MailRule `json:"rules"`
	}
	if err := s.do(ctx, http.MethodPost, "/reorder", map[string]any{"order": order}, &out); err != nil {
		return nil, err
	}
	return out.Rules, nil
}

func (s *httpRuleStore) Run(ctx context.Context, folder string, limit int) (int, int, error) {
	var out struct {
		Matched int `json:"matched"`
		Applied int `json:"applied"`
	}
	if err := s.do(ctx, http.MethodPost, "/run", map[string]any{"folder": folder, "limit": limit}, &out); err != nil {
		return 0, 0, err
	}
	return out.Matched, out.Applied, nil
}

// do performs one request against the rule store and decodes the JSON response
// into out (nil to ignore the body). Non-2xx responses become a *ruleAPIError
// carrying the upstream status and error message.
func (s *httpRuleStore) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set(hdrRulesAuth, s.secret)
	req.Header.Set(hdrRulesAccount, s.account)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return &ruleAPIError{status: http.StatusBadGateway, msg: "rule store unreachable"}
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
		return &ruleAPIError{status: resp.StatusCode, msg: msg}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &ruleAPIError{status: http.StatusBadGateway, msg: "invalid rule store response"}
		}
	}
	return nil
}
