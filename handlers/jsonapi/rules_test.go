package jsonapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// mockRuleStore records calls and returns canned data, standing in for the
// brokered HTTP client to vulos-mail's rule store.
type mockRuleStore struct {
	rules             []models.MailRule
	created           *models.MailRule
	createdIncomingID string
	updatedID         string
	deletedID   string
	reorderArgs []string
	runFolder   string
	runLimit    int
	runMatched  int
	runApplied  int
	err         error
}

func (m *mockRuleStore) List(context.Context) ([]models.MailRule, error) { return m.rules, m.err }
func (m *mockRuleStore) Create(_ context.Context, r models.MailRule) (models.MailRule, error) {
	if m.err != nil {
		return models.MailRule{}, m.err
	}
	m.createdIncomingID = r.ID // what the handler forwarded (should be empty)
	r.ID = "r_new"
	m.created = &r
	return r, nil
}
func (m *mockRuleStore) Update(_ context.Context, id string, r models.MailRule) (models.MailRule, error) {
	if m.err != nil {
		return models.MailRule{}, m.err
	}
	m.updatedID = id
	r.ID = id
	return r, nil
}
func (m *mockRuleStore) Delete(_ context.Context, id string) error { m.deletedID = id; return m.err }
func (m *mockRuleStore) Reorder(_ context.Context, order []string) ([]models.MailRule, error) {
	m.reorderArgs = order
	// return in the requested order for assertion
	out := make([]models.MailRule, 0, len(order))
	for _, id := range order {
		out = append(out, models.MailRule{ID: id})
	}
	return out, m.err
}
func (m *mockRuleStore) Run(_ context.Context, folder string, limit int) (int, int, error) {
	m.runFolder, m.runLimit = folder, limit
	return m.runMatched, m.runApplied, m.err
}

// rulesApp wires a brokered /v1 app whose rule store is the given mock. It sets
// a rules URL in the broker headers so ruleStoreFor resolves (the mock ignores
// the URL). Returns the app and the mock.
func rulesApp(t *testing.T, mock *mockRuleStore) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := newRuleStore
	newRuleStore = func(baseURL, secret, account string) ruleStore { return mock }
	t.Cleanup(func() { newRuleStore = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

func TestRules_List(t *testing.T) {
	mock := &mockRuleStore{rules: []models.MailRule{
		{ID: "r1", Name: "News", Enabled: true, Match: "all",
			Conditions: []models.RuleCondition{{Field: "from", Op: "contains", Value: "newsletter"}},
			Actions:    []models.RuleAction{{Type: "label", Value: "News"}}},
	}}
	app := rulesApp(t, mock)

	req := httptest.NewRequest("GET", "/v1/rules", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var out struct {
		Rules []models.MailRule `json:"rules"`
	}
	decode(t, resp.Body, &out)
	if len(out.Rules) != 1 || out.Rules[0].ID != "r1" {
		t.Fatalf("list body = %+v", out)
	}
}

func TestRules_Create_RoundTrip(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	rule := models.MailRule{
		Name: "Receipts", Enabled: true, Match: "all",
		Conditions: []models.RuleCondition{{Field: "subject", Op: "contains", Value: "receipt"}},
		Actions:    []models.RuleAction{{Type: "move", Value: "Receipts"}},
	}
	body, _ := json.Marshal(rule)
	req := httptest.NewRequest("POST", "/v1/rules", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if mock.created == nil || mock.created.Name != "Receipts" || mock.createdIncomingID != "" {
		t.Fatalf("create must forward the rule with no client id: incomingID=%q rule=%+v", mock.createdIncomingID, mock.created)
	}
	var out struct {
		Rule models.MailRule `json:"rule"`
	}
	decode(t, resp.Body, &out)
	if out.Rule.ID != "r_new" {
		t.Fatalf("create should return server id: %+v", out.Rule)
	}
}

func TestRules_RejectsForwardAction(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	rule := models.MailRule{
		Name: "leak", Enabled: true, Match: "all",
		Conditions: []models.RuleCondition{{Field: "from", Op: "contains", Value: "x"}},
		Actions:    []models.RuleAction{{Type: "forward", Value: "attacker@evil.example"}},
	}
	body, _ := json.Marshal(rule)
	req := httptest.NewRequest("POST", "/v1/rules", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("forward action: want 400, got %d", resp.StatusCode)
	}
	if mock.created != nil {
		t.Fatal("forbidden action must be rejected BEFORE reaching the store")
	}
}

func TestRules_Reorder(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	req := httptest.NewRequest("POST", "/v1/rules/reorder", strings.NewReader(`{"order":["b","a"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("reorder: %d", resp.StatusCode)
	}
	if strings.Join(mock.reorderArgs, ",") != "b,a" {
		t.Fatalf("reorder args = %v", mock.reorderArgs)
	}
}

func TestRules_Run(t *testing.T) {
	mock := &mockRuleStore{runMatched: 5, runApplied: 3}
	app := rulesApp(t, mock)

	req := httptest.NewRequest("POST", "/v1/rules/run", strings.NewReader(`{"folder":"INBOX","limit":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("run: %d", resp.StatusCode)
	}
	if mock.runFolder != "INBOX" || mock.runLimit != 100 {
		t.Fatalf("run args folder=%q limit=%d", mock.runFolder, mock.runLimit)
	}
	var out struct{ Matched, Applied int }
	decode(t, resp.Body, &out)
	if out.Matched != 5 || out.Applied != 3 {
		t.Fatalf("run body = %+v", out)
	}
}

func TestRules_Delete(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	req := httptest.NewRequest("DELETE", "/v1/rules/r1", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailRulesURL, "http://rules.internal/internal/mailrules")

	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if mock.deletedID != "r1" {
		t.Fatalf("delete id = %q", mock.deletedID)
	}
}

// When a brokered request carries NO rule-store URL (e.g. a plain Gmail account),
// the rules surface must report 501 so the mail-ui degrades gracefully.
func TestRules_UnsupportedWhenNoRulesURL(t *testing.T) {
	mock := &mockRuleStore{}
	app := rulesApp(t, mock)

	req := httptest.NewRequest("GET", "/v1/rules", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// deliberately NOT setting hdrMailRulesURL

	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("no rules URL: want 501, got %d", resp.StatusCode)
	}
}

func decode(t *testing.T, r io.Reader, v any) {
	t.Helper()
	b, _ := io.ReadAll(r)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("bad JSON: %s", b)
	}
}
