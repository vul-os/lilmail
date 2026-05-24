package main

import (
	"embed"
	"fmt"
	"io/fs"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/storage"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/template/html/v2"
)

//go:embed all:templates
var templatesFS embed.FS

//go:embed all:assets
var assetsFS embed.FS

var store *session.Store

func init() {
	// Create file storage
	storage, err := storage.NewFileStorage("./sessions")
	if err != nil {
		log.Fatal("Failed to initialize session storage:", err)
	}

	store = session.New(session.Config{
		Storage:        storage,
		Expiration:     24 * time.Hour,
		CookieSecure:   false, // Set to true in production with HTTPS
		CookieHTTPOnly: true,
	})
}

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
	engine.AddFunc("title", strings.Title)
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

	// Initialize web handlers
	webAuthHandler := web.NewAuthHandler(store, config)
	webEmailHandler := web.NewEmailHandler(store, config, webAuthHandler)
	webCalendarHandler := web.NewCalendarHandler(store, config, webAuthHandler)

	// Public routes
	app.Get("/login", webAuthHandler.ShowLogin)
	app.Post("/login", webAuthHandler.HandleLogin)
	app.Get("/logout", webAuthHandler.HandleLogout)

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
	}

	// Notifications routes — registered only when notifications.enabled = true.
	// With enabled = false (the default) this block is never entered, so no
	// extra goroutines are created and no new routes appear.
	if config.Notifications.Enabled {
		hub := web.NewNotificationHub(store, config, webAuthHandler)
		notifHandler := web.NewNotificationsHandler(hub)
		protected.Get("/events", notifHandler.HandleSSE)
	}

	// Calendar routes — registered only when CalDAV is enabled.
	if config.CalDAV.Enabled {
		protected.Get("/calendar", webCalendarHandler.HandleCalendarMonth)
		protected.Get("/calendar/week", webCalendarHandler.HandleCalendarWeek)
		protected.Get("/calendar/event/:uid", webCalendarHandler.HandleEventDetail)
		protected.Post("/calendar/event", webCalendarHandler.HandleCreateEvent)
		protected.Post("/calendar/rsvp", webCalendarHandler.HandleRSVP)
	}

	// HTMX routes (partial template renders)
	htmx := protected.Group("/htmx")
	{
		htmx.Get("/email/:id", webEmailHandler.HandleEmailView)
		htmx.Get("/folder/:name/emails", webEmailHandler.HandleFolderEmails)
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
