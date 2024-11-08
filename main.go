// main.go (relevant parts)

package main

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers"
	"lilmail/utils"
	"log"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/template/html/v2"
)

var store *session.Store

func init() {
	store = session.New(session.Config{
		Expiration:     24 * time.Hour,
		CookieSecure:   false, // Set to true in production with HTTPS
		CookieHTTPOnly: true,
	})
}

func main() {
	// Load configuration
	config, err := config.LoadConfig("config.toml")
	if err != nil {
		log.Fatal("Failed to load config:", err)
	}

	// Initialize template engine
	engine := html.New("./templates", ".html")
	engine.AddFunc("formatDate", func(t time.Time) string {
		return t.Format("Jan 02, 2006 15:04")
	})

	// Initialize Fiber with template engine
	app := fiber.New(fiber.Config{
		Views: engine,
	})

	// Serve static files
	app.Static("/assets", "./assets")

	// Initialize handlers
	authHandler := handlers.NewAuthHandler(store, config)
	emailHandler := handlers.NewEmailHandler(store, config)

	// Public routes
	app.Get("/login", authHandler.ShowLogin)
	app.Post("/login", authHandler.HandleLogin)
	app.Get("/logout", authHandler.HandleLogout)

	// Protected routes group
	protected := app.Group("", handlers.AuthMiddleware(store))

	protected.Get("/inbox", emailHandler.HandleInbox)
	protected.Get("/email/:id", emailHandler.HandleSingleEmail)
	protected.Get("/folder/:name", emailHandler.HandleFolder)
	protected.Get("/api/folder/:name/emails", emailHandler.HandleFolderEmails)

	// Start server
	log.Fatal(app.Listen(":3000"))
}

// handlers/auth.go (middleware part)

// AuthMiddleware checks if the user is authenticated
func AuthMiddleware(store *session.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Get session
		sess, err := store.Get(c)
		if err != nil {
			return c.Redirect("/login")
		}

		// Check authentication
		auth := sess.Get("authenticated")
		if auth == nil || auth != true {
			return c.Redirect("/login")
		}

		// Get user data
		username := sess.Get("username")
		email := sess.Get("email")

		// Validate required session data
		if username == nil || email == nil {
			sess.Destroy()
			return c.Redirect("/login")
		}

		// Set data in context
		c.Locals("username", username)
		c.Locals("email", email)
		c.Locals("authenticated", true)

		return c.Next()
	}
}

// handlers/email.go (folder handler)

type EmailHandler struct {
	store  *session.Store
	config *config.Config
}

func (h *EmailHandler) HandleFolder(c *fiber.Ctx) error {
	// Get username from context (set by middleware)
	username := c.Locals("username")
	if username == nil {
		return c.Redirect("/login")
	}

	usernameStr, ok := username.(string)
	if !ok {
		return c.Redirect("/login")
	}

	folderName := c.Params("name")
	if folderName == "" {
		return c.Redirect("/inbox")
	}

	// Load folders for sidebar
	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)
	var folders []*handlers.MailboxInfo
	err := utils.LoadCache(filepath.Join(userCacheFolder, "folders.json"), &folders)
	if err != nil {
		return c.Status(500).SendString("Error loading folders")
	}

	// Get IMAP client and fetch folder emails
	client, err := h.getIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	emails, err := client.FetchMessages(folderName, 10)
	if err != nil {
		return c.Status(500).SendString("Error fetching emails")
	}

	return c.Render("inbox", fiber.Map{
		"Username":      usernameStr,
		"Folders":       folders,
		"Emails":        emails,
		"CurrentFolder": folderName,
	})
}

// Helper method to get IMAP client
func (h *EmailHandler) getIMAPClient(c *fiber.Ctx) (*handlers.Client, error) {
	email := c.Locals("email")
	if email == nil {
		return nil, fmt.Errorf("email not found in session")
	}

	emailStr, ok := email.(string)
	if !ok {
		return nil, fmt.Errorf("invalid email in session")
	}

	sess, err := h.store.Get(c)
	if err != nil {
		return nil, err
	}

	// You might want to store/retrieve password differently in production
	// This is just an example
	password := sess.Get("password")
	if password == nil {
		return nil, fmt.Errorf("password not found in session")
	}

	passwordStr, ok := password.(string)
	if !ok {
		return nil, fmt.Errorf("invalid password in session")
	}

	return handlers.NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		emailStr,
		passwordStr,
	)
}
