package web_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
)

// TestLoginRateLimitTriggers verifies that the login rate-limiter returns 429
// once the configured maximum is exceeded. The test uses a minimal Fiber app
// with only the limiter middleware and a stub handler — identical to the wiring
// in main.go — so it validates the limiter configuration rather than the full
// auth stack.
func TestLoginRateLimitTriggers(t *testing.T) {
	const maxAttempts = 3

	app := fiber.New()
	loginLimiter := limiter.New(limiter.Config{
		Max:        maxAttempts,
		Expiration: 60 * time.Second,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).SendString("rate limited")
		},
	})
	app.Post("/login", loginLimiter, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	// First maxAttempts requests should all succeed.
	for i := 1; i <= maxAttempts; i++ {
		req := httptest.NewRequest("POST", "/login", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("request %d: want 200, got %d", i, resp.StatusCode)
		}
	}

	// The next request must be rate-limited.
	req := httptest.NewRequest("POST", "/login", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("rate-limit request: %v", err)
	}
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Errorf("rate-limit request: want 429, got %d", resp.StatusCode)
	}
}
