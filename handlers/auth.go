// handlers/auth.go
package handlers

import (
	"fmt"
	"lilmail/config"
	"lilmail/utils"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

type AuthHandler struct {
	store  *session.Store
	config *config.Config
}

// NewAuthHandler creates a new instance of AuthHandler
func NewAuthHandler(store *session.Store, config *config.Config) *AuthHandler {
	return &AuthHandler{
		store:  store,
		config: config,
	}
}

// AuthMiddleware checks if the user is authenticated
func AuthMiddleware(store *session.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		sess, err := store.Get(c)
		if err != nil {
			return c.Redirect("/login")
		}

		authenticated := sess.Get("authenticated")
		if authenticated == nil || authenticated != true {
			return c.Redirect("/login")
		}

		// Set user data in context for use in handlers
		username := sess.Get("username")
		if username != nil {
			c.Locals("username", username)
		}
		email := sess.Get("email")
		if email != nil {
			c.Locals("email", email)
		}

		return c.Next()
	}
}

// ShowLogin renders the login page
func (h *AuthHandler) ShowLogin(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err == nil {
		authenticated := sess.Get("authenticated")
		if authenticated == true {
			return c.Redirect("/inbox")
		}
	}
	return c.Render("login", fiber.Map{})
}

// HandleLogin processes the login form
func (h *AuthHandler) HandleLogin(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err != nil {
		return c.Status(500).SendString("Session error")
	}

	// Get form values
	email := strings.TrimSpace(c.FormValue("email"))
	password := strings.TrimSpace(c.FormValue("password"))

	// Validate input
	if email == "" || password == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Email and password are required",
			"Email": email,
		})
	}

	// Extract username from email
	username := h.getUsernameFromEmail(email)
	if username == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Invalid email format",
			"Email": email,
		})
	}
	fmt.Println(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		email,
		password,
	)
	// Attempt IMAP login
	client, err := NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		username,
		password,
	)
	if err != nil {
		return c.Status(401).Render("login", fiber.Map{
			"Error": "Invalid credentials or server error",
			"Email": email,
		})
	}
	defer client.Close()

	// Create user cache directory
	userCacheFolder := filepath.Join(h.config.Cache.Folder, username)
	if err := h.ensureUserCacheFolder(userCacheFolder); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Server error occurred during setup",
			"Email": email,
		})
	}

	// Store session data
	sess.Set("authenticated", true)
	sess.Set("email", email)
	sess.Set("username", username)

	// Set session expiry (24 hours)
	sess.SetExpiry(24 * 60 * 60 * time.Second)

	if err := sess.Save(); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to create session",
			"Email": email,
		})
	}

	// Fetch initial data
	if err := h.fetchInitialData(client, userCacheFolder); err != nil {
		// Log the error but don't fail the login - data can be fetched later
		fmt.Printf("Error fetching initial data for user %s: %v\n", username, err)
	}

	return c.Redirect("/inbox")
}

// HandleLogout processes user logout
func (h *AuthHandler) HandleLogout(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err != nil {
		return c.Redirect("/login")
	}

	// Get username before destroying session
	username := sess.Get("username")
	if username != nil {
		userStr, ok := username.(string)
		if ok {
			// Clear user cache
			userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
			if err := h.clearUserCache(userCacheFolder); err != nil {
				fmt.Printf("Error clearing cache for user %s: %v\n", userStr, err)
			}
		}
	}

	// Destroy session
	if err := sess.Destroy(); err != nil {
		return c.Status(500).SendString("Error during logout")
	}

	return c.Redirect("/login")
}

// Helper functions

func (h *AuthHandler) getUsernameFromEmail(email string) string {
	parts := strings.Split(strings.TrimSpace(email), "@")
	if len(parts) == 2 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

func (h *AuthHandler) ensureUserCacheFolder(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.MkdirAll(path, 0755)
	}
	return nil
}

func (h *AuthHandler) clearUserCache(path string) error {
	if path == "" {
		return nil
	}

	dir, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer dir.Close()

	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range names {
		err = os.RemoveAll(filepath.Join(path, name))
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *AuthHandler) fetchInitialData(client *Client, cacheFolder string) error {
	if client == nil {
		return fmt.Errorf("invalid IMAP client")
	}

	// Fetch folders
	folders, err := client.FetchFolders()
	if err != nil {
		return fmt.Errorf("failed to fetch folders: %v", err)
	}

	// Cache folders
	if err := utils.SaveCache(filepath.Join(cacheFolder, "folders.json"), folders); err != nil {
		return fmt.Errorf("failed to cache folders: %v", err)
	}

	// Fetch inbox messages
	messages, err := client.FetchMessages("INBOX", 10)
	if err != nil {
		return fmt.Errorf("failed to fetch messages: %v", err)
	}

	// Cache messages
	if err := utils.SaveCache(filepath.Join(cacheFolder, "emails.json"), messages); err != nil {
		return fmt.Errorf("failed to cache messages: %v", err)
	}

	return nil
}

// GetSessionUser safely retrieves username from context
func GetSessionUser(c *fiber.Ctx) string {
	if username := c.Locals("username"); username != nil {
		if usernameStr, ok := username.(string); ok {
			return usernameStr
		}
	}
	return ""
}

// GetSessionEmail safely retrieves email from context
func GetSessionEmail(c *fiber.Ctx) string {
	if email := c.Locals("email"); email != nil {
		if emailStr, ok := email.(string); ok {
			return emailStr
		}
	}
	return ""
}

// ValidateSession checks if the current session is valid
func (h *AuthHandler) ValidateSession(c *fiber.Ctx) (*session.Session, error) {
	sess, err := h.store.Get(c)
	if err != nil {
		return nil, err
	}

	authenticated := sess.Get("authenticated")
	if authenticated == nil || authenticated != true {
		return nil, fmt.Errorf("session not authenticated")
	}

	return sess, nil
}

// RefreshSession extends the session lifetime
func (h *AuthHandler) RefreshSession(sess *session.Session) error {
	if sess == nil {
		return fmt.Errorf("invalid session")
	}
	sess.SetExpiry(24 * 60 * 60 * time.Second)
	return sess.Save()
}
