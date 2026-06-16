package main

import (
	"embed"
	"fmt"
	"io/fs"
	"lilmail/config"
	"lilmail/handlers/ai"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/storage"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/template/html/v2"
)

// titleCase converts the first letter of each word to upper-case.
// It is used as a template function and replaces the deprecated strings.Title.
func titleCase(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			upper = true
			b.WriteRune(r)
		} else if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

//go:embed all:templates
var templatesFS embed.FS

//go:embed all:assets
var assetsFS embed.FS

var store *session.Store

// Helper function to determine if request is an API request
func isAPIRequest(c *fiber.Ctx) bool {
	if c == nil {
		return false
	}

	// Check for HTMX request first
	if c.Get("HX-Request") != "" {
		return true
	}

	// Safely check if path starts with /api
	path := c.Path()
	return len(path) >= 4 && path[:4] == "/api"
}

func main() {
	// Load configuration
	config, err := config.LoadConfig("config.toml")
	if err != nil {
		log.Fatal("Failed to load config:", err)
	}

	// Initialize session store now that we have the config (CookieSecure needs it).
	{
		fileStorage, err := storage.NewFileStorage("./sessions")
		if err != nil {
			log.Fatal("Failed to initialize session storage:", err)
		}
		store = session.New(session.Config{
			Storage:        fileStorage,
			Expiration:     24 * time.Hour,
			CookieSecure:   config.Server.SecureCookies, // true in TLS-terminated deployments
			CookieHTTPOnly: true,
			CookieSameSite: "Lax", // Prevents CSRF via cross-site form submissions
		})
	}

	// Initialize template engine with embedded filesystem
	tplSub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		log.Fatal("Failed to sub templates FS:", err)
	}
	engine := html.NewFileSystem(http.FS(tplSub), ".html")

	// String manipulation functions
	engine.AddFunc("split", strings.Split)
	engine.AddFunc("join", strings.Join)
	engine.AddFunc("lower", strings.ToLower)
	engine.AddFunc("upper", strings.ToUpper)
	engine.AddFunc("title", titleCase)
	engine.AddFunc("trim", strings.TrimSpace)
	engine.AddFunc("hasPrefix", strings.HasPrefix)
	engine.AddFunc("urlEncode", url.QueryEscape)

	// Date formatting function
	engine.AddFunc("formatDate", func(t time.Time) string {
		return t.Format("Jan 02, 2006 15:04")
	})

	// File size formatting function. Accepts int (models.Attachment.Size) — the
	// template passes an int, and text/template will not coerce int -> int64.
	engine.AddFunc("formatSize", func(sizeInt int) string {
		size := int64(sizeInt)
		const unit = 1024
		if size < unit {
			return fmt.Sprintf("%d B", size)
		}
		div, exp := int64(unit), 0
		for n := size / unit; n >= unit; n /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
	})

	// initial returns the first letter (unicode-safe, upper-cased) of the
	// preferred name, falling back to the email, for avatar bubbles.
	engine.AddFunc("initial", func(name, email string) string {
		s := strings.TrimSpace(name)
		if s == "" {
			s = strings.TrimSpace(email)
		}
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	})

	// caldavEnabled is a zero-argument template func so templates can show/hide
	// calendar navigation without threading a flag through every handler.
	engine.AddFunc("caldavEnabled", func() bool {
		return config.CalDAV.Enabled
	})

	// notificationsEnabled is a zero-argument template func so templates can
	// conditionally emit the SSE client script.  It is always registered (so
	// the template parse step never fails) but returns false by default.
	engine.AddFunc("notificationsEnabled", func() bool {
		return config.Notifications.Enabled
	})

	// webPushEnabled tells templates whether to register the service worker and
	// expose the "Enable push notifications" button.
	engine.AddFunc("webPushEnabled", func() bool {
		return config.Notifications.Enabled && config.Notifications.WebPush
	})

	// accountsEnabled tells templates whether to show the account-switcher UI.
	engine.AddFunc("accountsEnabled", func() bool {
		return config.Accounts.Enabled
	})

	engine.Reload(false) // embedded — no disk reload needed

	// Initialize Fiber with template engine
	app := fiber.New(fiber.Config{
		Views:       engine,
		ViewsLayout: "layouts/main",   // Default layout
		BodyLimit:   25 * 1024 * 1024, // 25 MiB — guards compose form uploads
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}

			// Handle API requests differently
			if isAPIRequest(c) {
				return c.Status(code).JSON(fiber.Map{
					"error": err.Error(),
				})
			}

			// Render error page for regular requests
			return c.Status(code).Render("error", fiber.Map{
				"Error": err.Error(),
				"Code":  code,
			})
		},
	})

	// Add middleware
	app.Use(recover.New()) // Recover from panics
	app.Use(logger.New())  // Request logging

	// Apply security headers on every response.
	app.Use(func(c *fiber.Ctx) error {
		for h, v := range config.GetSecurityHeaders() {
			c.Set(h, v)
		}
		return c.Next()
	})

	// Serve embedded assets
	assetsSub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		log.Fatal("Failed to sub assets FS:", err)
	}
	app.Use("/assets", filesystem.New(filesystem.Config{
		Root:         http.FS(assetsSub),
		MaxAge:       int(24 * time.Hour / time.Second),
		NotFoundFile: "",
	}))

	// Service worker must be served at the root scope (not under /assets/)
	// so that it can intercept fetch requests and show push notifications for
	// the entire origin.  Served with Cache-Control: no-cache so the browser
	// always picks up updates promptly.
	app.Get("/sw.js", func(c *fiber.Ctx) error {
		swBytes, readErr := assetsFS.ReadFile("assets/sw.js")
		if readErr != nil {
			return fiber.ErrNotFound
		}
		c.Set("Content-Type", "application/javascript; charset=utf-8")
		c.Set("Cache-Control", "no-cache")
		c.Set("Service-Worker-Allowed", "/") // Allow SW to control the full origin.
		return c.Send(swBytes)
	})

	// Initialize web handlers
	webAuthHandler := web.NewAuthHandler(store, config)
	webEmailHandler := web.NewEmailHandler(store, config, webAuthHandler)
	webCalendarHandler := web.NewCalendarHandler(store, config, webAuthHandler)

	// Public routes
	app.Get("/login", webAuthHandler.ShowLogin)
	app.Post("/login", webAuthHandler.HandleLogin)
	app.Get("/logout", webAuthHandler.HandleLogout)

	// Demo / screenshot mode — registered only when [demo] enabled = true.
	// Both GET and POST /demo-login immediately establish a demo session
	// (no IMAP contact) and redirect to /inbox. This lets Playwright simply
	// navigate to /demo-login and follow the redirect.
	if config.Demo.Enabled {
		app.Get("/demo-login", webAuthHandler.HandleDemoLogin)
		app.Post("/demo-login", webAuthHandler.HandleDemoLogin)
	}

	// OAuth2 login routes (public; the callback establishes the session)
	if config.OAuth2.Enabled {
		app.Get("/auth/oauth/login", webAuthHandler.HandleOAuthLogin)
		app.Get("/auth/oauth/callback", webAuthHandler.HandleOAuthCallback)
	}

	// Protected routes group
	protected := app.Group("", api.SessionMiddleware(store))

	// Main web routes
	protected.Get("/", webEmailHandler.HandleInbox)      // Default to inbox
	protected.Get("/inbox", webEmailHandler.HandleInbox) // Explicit inbox route
	protected.Get("/folder/:name", webEmailHandler.HandleFolder)

	// API routes - Keep these paths exactly as they were before
	apiRoutes := protected.Group("/api")
	{
		// Email routes
		apiRoutes.Get("/email/:id", webEmailHandler.HandleEmailView)
		apiRoutes.Delete("/email/:id", webEmailHandler.HandleDeleteEmail)

		// Attachment download (ID encodes folder + UID + MIME part)
		apiRoutes.Get("/attachment/:id", webEmailHandler.HandleAttachment)

		// Folder routes - This is the important fix
		apiRoutes.Get("/folder/:name/emails", webEmailHandler.HandleFolderEmails) // Match the path in HTML

		// Composition routes
		apiRoutes.Post("/compose", webEmailHandler.HandleComposeEmail)

		// Drafts
		apiRoutes.Post("/draft", webEmailHandler.HandleSaveDraft)
		apiRoutes.Get("/drafts", webEmailHandler.HandleListDrafts)

		// Recipient autocomplete (recent senders + optional CardDAV)
		apiRoutes.Get("/autocomplete", webEmailHandler.HandleAutocomplete)

		// Mark-as-unread: removes the \Seen flag from a message.
		apiRoutes.Patch("/email/:id/unread", webEmailHandler.HandleMarkUnread)

		// Search: IMAP SEARCH returning an email-list partial.
		apiRoutes.Get("/search", webEmailHandler.HandleSearch)
	}

	// AI mail-assistant routes — registered always (gated internally on config.AI.Enabled).
	// When disabled, all /api/ai/* routes return 404 {"error":"ai_disabled"}.
	ai.RegisterRoutes(apiRoutes, config.AI)

	// Notifications routes — registered only when notifications.enabled = true.
	// With enabled = false (the default) this block is never entered, so no
	// extra goroutines are created and no new routes appear.
	if config.Notifications.Enabled {
		// Optional VAPID Web Push.
		var vapidKeys *web.VAPIDKeys
		var pushStore *web.PushStore
		if config.Notifications.WebPush {
			var err error
			vapidKeys, err = web.LoadOrGenerateVAPIDKeys(config.Notifications.VAPIDKeyFile)
			if err != nil {
				log.Printf("webpush: VAPID key init failed (%v) — web push disabled", err)
			} else {
				log.Printf("webpush: VAPID public key loaded (%s)", config.Notifications.VAPIDKeyFile)
				cacheRoot := config.Cache.Folder
				if cacheRoot == "" {
					cacheRoot = "."
				}
				pushStore = web.NewPushStore(cacheRoot)
			}
		}

		hub := web.NewNotificationHub(store, config, webAuthHandler, vapidKeys, pushStore)
		notifHandler := web.NewNotificationsHandler(hub)
		protected.Get("/events", notifHandler.HandleSSE)

		// VAPID public key endpoint — public (no session required) so the SW can
		// fetch it before the user navigates to an authenticated page.
		if vapidKeys != nil {
			pushHandler := web.NewPushHandler(vapidKeys, pushStore)
			app.Get("/api/push/vapid-public", pushHandler.HandleVAPIDPublicKey)
			protected.Post("/api/push/subscribe", pushHandler.HandleSubscribe)
			protected.Delete("/api/push/subscribe", pushHandler.HandleUnsubscribe)
		}
	}

	// Multi-account routes — registered only when accounts.enabled = true.
	var acctHandler *web.AccountsHandler
	if config.Accounts.Enabled {
		acctStore, err := web.OpenAccountStore(config.Accounts.StoreFile)
		if err != nil {
			log.Fatalf("accounts: open store: %v", err)
		}
		// Wire the account store into the email handler so unified-inbox fetches work.
		webEmailHandler.SetAccountStore(acctStore)

		acctHandler = web.NewAccountsHandler(store, config, webAuthHandler, acctStore)

		protected.Get("/api/accounts", acctHandler.HandleListAccounts)
		protected.Post("/api/accounts", acctHandler.HandleAddAccount)
		protected.Delete("/api/accounts/:email", acctHandler.HandleDeleteAccount)
		protected.Post("/api/accounts/:email/switch", acctHandler.HandleSwitchAccount)
	}
	// Settings page — always registered so users can reach it even without extras.
	// When accounts.enabled = false the settings page shows a placeholder panel.
	if acctHandler != nil {
		protected.Get("/settings", acctHandler.HandleSettings)
	} else {
		// Minimal settings handler when accounts are disabled.
		protected.Get("/settings", func(c *fiber.Ctx) error {
			username, _ := c.Locals("username").(string)
			email, _ := c.Locals("email").(string)
			sess, _ := store.Get(c)
			token, _ := sess.Get("token").(string)
			return c.Render("settings", fiber.Map{
				"Title":           "Settings",
				"Username":        username,
				"Email":           email,
				"Token":           token,
				"AccountsEnabled": false,
			})
		})
	}

	// Calendar routes — registered only when CalDAV is enabled.
	if config.CalDAV.Enabled {
		protected.Get("/calendar", webCalendarHandler.HandleCalendarMonth)
		protected.Get("/calendar/week", webCalendarHandler.HandleCalendarWeek)
		protected.Get("/calendar/event/:uid", webCalendarHandler.HandleEventDetail)
		protected.Post("/calendar/event", webCalendarHandler.HandleCreateEvent)
		protected.Post("/calendar/rsvp", webCalendarHandler.HandleRSVP)
	}

	// Health check endpoint
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	// 404 Handler for undefined routes
	app.Use(func(c *fiber.Ctx) error {
		if isAPIRequest(c) {
			return c.Status(404).JSON(fiber.Map{
				"error": "Not Found",
			})
		}
		return c.Status(404).Render("error", fiber.Map{
			"Error": "Page not found",
			"Code":  404,
		})
	})

	// Start server
	log.Printf("Starting server on port %d...\n", config.Server.Port)
	if err := app.Listen(fmt.Sprintf(":%d", config.Server.Port)); err != nil {
		log.Fatal("Error starting server: ", err)
	}
}
