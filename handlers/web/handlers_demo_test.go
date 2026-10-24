package web

import (
	"lilmail/config"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/valyala/fasthttp"
)

// demoApp wires the real session middleware and handlers around demo mode (an
// in-memory MailClient), so handlers can be driven end to end without IMAP.
func demoApp(t *testing.T) (*fiber.App, *EmailHandler, *session.Store) {
	t.Helper()
	cfg := &config.Config{}
	cfg.JWT.Secret = "test-secret-test-secret-test-secret"
	cfg.Cache.Folder = t.TempDir()
	store := session.New()
	auth := NewAuthHandler(store, cfg)
	eh := NewEmailHandler(store, cfg, auth)

	app := fiber.New()
	// Test-only: establish a demo session (what HandleDemoLogin does).
	app.Get("/_login", func(c *fiber.Ctx) error {
		sess, err := store.Get(c)
		if err != nil {
			return err
		}
		sess.Set("authenticated", true)
		sess.Set("email", "demo@lilmail.dev")
		sess.Set("username", "demo")
		sess.Set("auth_type", demoAuthType)
		return sess.Save()
	})
	return app, eh, store
}

func loginCookie(t *testing.T, app *fiber.App) string {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/_login", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		return c.Name + "=" + c.Value
	}
	t.Fatal("no session cookie")
	return ""
}

// Regression: Subscribe used to start a goroutine that touched the Fiber ctx
// after the handler returned (Fiber recycles it): a nil dereference that killed
// the whole process on the first /events subscription. The IMAP client must be
// created inside the request. Here the ctx is released right after Subscribe
// returns, exactly as Fiber does when a handler finishes; a goroutine that still
// uses it panics and fails the whole test binary.
func TestSubscribe_DoesNotKeepFiberCtx(t *testing.T) {
	app, eh, store := demoApp(t)
	cfg := eh.config
	cfg.Notifications.Enabled, cfg.Notifications.Idle = true, true
	hub := NewNotificationHub(store, cfg, eh.auth, nil, nil)
	cookie := loginCookie(t, app)

	ch := make(chan MailEvent, 1)
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.Set("Cookie", cookie)
	c := app.AcquireCtx(fctx)
	hub.Subscribe("demo", ch, c)
	app.ReleaseCtx(c) // the handler "returned": Fiber recycles c
	time.Sleep(150 * time.Millisecond)

	hub.mu.Lock()
	st := hub.users["demo"]
	running := st != nil && st.stop != nil
	hub.mu.Unlock()
	if !running {
		t.Fatal("watcher should be running after Subscribe")
	}
	hub.Unsubscribe("demo", ch)
	time.Sleep(50 * time.Millisecond)
	hub.mu.Lock()
	_, still := hub.users["demo"]
	hub.mu.Unlock()
	if still {
		t.Fatal("hub should drop the user once the last subscriber leaves")
	}
}
