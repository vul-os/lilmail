package main

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers"
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
	// Add to your template functions
	engine.AddFunc("hasPrefix", strings.HasPrefix)
	// Add template functions
	engine.AddFunc("formatDate", func(t time.Time) string {
		return t.Format("Jan 02, 2006 15:04")
	})

	engine.AddFunc("split", strings.Split)

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
			// Recover from panic if any
			// if err := recover(); err != nil {
			// 	log.Printf("Panic recovered: %v", err)
			// 	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			// 		"error": "Internal server error",
			// 	})
			// }

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

	// Serve static files
	app.Static("/assets", "./assets", fiber.Static{
		Compress:      true,
		CacheDuration: 24 * time.Hour,
	})

	// Initialize handlers
	authHandler := handlers.NewAuthHandler(store, config)
	emailHandler := handlers.NewEmailHandler(store, config, authHandler)

	// Public routes
	app.Get("/login", authHandler.ShowLogin)
	app.Post("/login", authHandler.HandleLogin)
	app.Get("/logout", authHandler.HandleLogout)

	// Protected routes group
	protected := app.Group("", handlers.AuthMiddleware(store))

	// Main routes
	protected.Get("/", emailHandler.HandleInbox)      // Default to inbox
	protected.Get("/inbox", emailHandler.HandleInbox) // Explicit inbox route
	protected.Get("/folder/:name", emailHandler.HandleFolder)

	// API routes
	api := protected.Group("/api")
	{
		// Email routes
		api.Get("/email/:id", emailHandler.HandleEmailView)
		api.Delete("/email/:id", emailHandler.HandleDeleteEmail)

		// Folder routes
		api.Get("/folder/:name/emails", emailHandler.HandleFolderEmails)

		// Composition routes
		api.Post("/compose", emailHandler.HandleComposeEmail)
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
