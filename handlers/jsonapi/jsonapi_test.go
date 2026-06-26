package jsonapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"lilmail/config"
	"lilmail/handlers/web"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// Unauthenticated requests must get 401 JSON (never an HTML redirect), so a
// fetch()-based client can react programmatically. This covers the read routes
// plus the additive compose/send routes.
func TestRequireAuthReturnsJSON401(t *testing.T) {
	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))

	app := fiber.New()
	h.Register(app)

	type tc struct {
		method, path string
	}
	cases := []tc{
		{"GET", "/v1/folders"},
		{"GET", "/v1/messages"},
		{"GET", "/v1/me"},
		{"POST", "/v1/messages"},
		{"POST", "/v1/messages/42/move"},
		{"POST", "/v1/drafts"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("%s %s: want 401, got %d", c.method, c.path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s %s: response not JSON: %s", c.method, c.path, body)
		}
		if _, ok := out["error"]; !ok {
			t.Fatalf("%s %s: JSON missing error field: %s", c.method, c.path, body)
		}
	}
}

// hasRoute reports whether any registered route matches the given path.
func hasRoute(app *fiber.App, path string) bool {
	for _, r := range app.GetRoutes() {
		if r.Path == path {
			return true
		}
	}
	return false
}

// Calendar/contacts routes are config-gated: only registered when their
// integration is enabled in config.
func TestCalendarContactsAreConfigGated(t *testing.T) {
	store := session.New()
	gated := []string{"/v1/calendar/events", "/v1/calendar/freebusy", "/v1/contacts"}

	// Disabled: gated routes must NOT be registered.
	off := New(store, &config.Config{}, web.NewAuthHandler(store, &config.Config{}))
	appOff := fiber.New()
	off.Register(appOff)
	for _, p := range gated {
		if hasRoute(appOff, p) {
			t.Fatalf("%s: registered while integration disabled", p)
		}
	}
	// The always-on compose + move routes must always be registered.
	if !hasRoute(appOff, "/v1/messages") || !hasRoute(appOff, "/v1/drafts") ||
		!hasRoute(appOff, "/v1/messages/:uid/move") {
		t.Fatalf("compose/draft/move routes missing")
	}

	// Enabled: gated routes must be registered.
	cfg := &config.Config{}
	cfg.CalDAV.Enabled = true
	cfg.CardDAV.Enabled = true
	on := New(store, cfg, web.NewAuthHandler(store, cfg))
	appOn := fiber.New()
	on.Register(appOn)
	for _, p := range gated {
		if !hasRoute(appOn, p) {
			t.Fatalf("%s: not registered while integration enabled", p)
		}
	}
}
