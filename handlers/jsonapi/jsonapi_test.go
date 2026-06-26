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
// fetch()-based client can react programmatically.
func TestRequireAuthReturnsJSON401(t *testing.T) {
	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))

	app := fiber.New()
	h.Register(app)

	for _, path := range []string{"/v1/folders", "/v1/messages", "/v1/me"} {
		req := httptest.NewRequest("GET", path, nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: response not JSON: %s", path, body)
		}
		if _, ok := out["error"]; !ok {
			t.Fatalf("%s: JSON missing error field: %s", path, body)
		}
	}
}
