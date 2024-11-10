package main

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/storage"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/template/html/v2"
)

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

	// Initialize template engine with custom functions
	engine := html.New("./templates", ".html")

	// String manipulation functions
	engine.AddFunc("split", strings.Split)
	engine.AddFunc("join", strings.Join)
	engine.AddFunc("lower", strings.ToLower)
	engine.AddFunc("upper", strings.ToUpper)
	engine.AddFunc("title", strings.Title)
	engine.AddFunc("trim", strings.TrimSpace)
	engine.AddFunc("hasPrefix", strings.HasPrefix)

	// Date formatting function
	engine.AddFunc("formatDate", func(t time.Time) string {
		return t.Format("Jan 02, 2006 15:04")
	})

	// File size formatting function
	engine.AddFunc("formatSize", func(size int64) string {
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

	engine.Reload(true)

	// Initialize Fiber with template engine
	app := fiber.New(fiber.Config{
		Views:       engine,
		ViewsLayout: "layouts/main", // Default layout
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

	// Add security headers middleware if SSL is enabled
	if config.SSL.Enabled {
		app.Use(func(c *fiber.Ctx) error {
			for header, value := range config.GetSecurityHeaders() {
				c.Set(header, value)
			}
			return c.Next()
		})

		// Start HTTPS server
		log.Printf("Starting HTTPS server on port %d...\n", config.SSL.Port)

		// If auto-redirect is enabled, configure the app to listen on both ports
		if config.SSL.AutoRedirect {
			// Create redirect handler for HTTP
			go func() {
				// Use the same app instance but only for redirects on HTTP port
				if err := app.Listen(fmt.Sprintf(":%d", config.SSL.HTTPPort)); err != nil {
					log.Printf("Warning: HTTP redirect server failed: %v", err)
				}
			}()

			// Add middleware to redirect HTTP to HTTPS
			app.Use(func(c *fiber.Ctx) error {
				if !c.Secure() {
					return c.Redirect("https://" + c.Hostname() + c.OriginalURL())
				}
				return c.Next()
			})
		}

		// Start the main HTTPS server
		if err := app.ListenTLS(
			fmt.Sprintf(":%d", config.SSL.Port),
			config.SSL.CertFile,
			config.SSL.KeyFile,
		); err != nil {
			log.Fatal("Error starting HTTPS server: ", err)
		}
	} else {
		// Start regular HTTP server
		log.Printf("Starting HTTP server on port %d...\n", config.SSL.HTTPPort)
		if err := app.Listen(fmt.Sprintf(":%d", config.SSL.HTTPPort)); err != nil {
			log.Fatal("Error starting HTTP server: ", err)
		}
	}

	// Add middleware
	app.Use(recover.New()) // Recover from panics
	app.Use(logger.New())  // Request logging

	// Serve static files
	app.Static("/assets", "./assets", fiber.Static{
		Compress:      true,
		CacheDuration: 24 * time.Hour,
	})

	// Initialize web handlers
	webAuthHandler := web.NewAuthHandler(store, config)
	webEmailHandler := web.NewEmailHandler(store, config, webAuthHandler)

	// Public routes
	app.Get("/login", webAuthHandler.ShowLogin)
	app.Post("/login", webAuthHandler.HandleLogin)
	app.Get("/logout", webAuthHandler.HandleLogout)

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

		// Folder routes - This is the important fix
		apiRoutes.Get("/folder/:name/emails", webEmailHandler.HandleFolderEmails) // Match the path in HTML

		// Composition routes
		apiRoutes.Post("/compose", webEmailHandler.HandleComposeEmail)
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
	port := 3000 // default port
	log.Printf("Starting server on port %d...\n", port)
	if err := app.Listen(fmt.Sprintf(":%d", port)); err != nil {
		log.Fatal("Error starting server: ", err)
	}
}
