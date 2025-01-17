// handlers/web/auth.go
package web

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/utils"
	"log"
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
	client *api.Client
}

// NewAuthHandler creates a new instance of AuthHandler
func NewAuthHandler(store *session.Store, config *config.Config) *AuthHandler {
	return &AuthHandler{
		store:  store,
		config: config,
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

	email := strings.TrimSpace(c.FormValue("email"))
	password := strings.TrimSpace(c.FormValue("password"))

	if email == "" || password == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Email and password are required",
			"Email": email,
		})
	}
	var username string

	if h.config.Server.UsernameIsEmail {
		username = email
	} else {
		username = api.GetUsernameFromEmail(email)
	}
	log.Println("Username:", username)
	if username == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Invalid email format",
			"Email": email,
		})
	}

	client, err := api.NewClient(
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

	userCacheFolder := filepath.Join(h.config.Cache.Folder, username)
	if err := h.ensureUserCacheFolder(userCacheFolder); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Server error occurred during setup",
			"Email": email,
		})
	}

	token, err := api.GenerateToken(username, email, h.config.JWT.Secret)
	if err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to create authentication token",
			"Email": email,
		})
	}

	encryptedCreds, err := api.EncryptCredentials(email, password, h.config.Encryption.Key)
	if err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to secure credentials",
			"Email": email,
		})
	}

	sess.Set("authenticated", true)
	sess.Set("email", email)
	sess.Set("username", username)
	sess.Set("token", token)
	sess.Set("credentials", encryptedCreds)
	sess.SetExpiry(24 * 60 * 60 * time.Second)

	if err := sess.Save(); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to create session",
			"Email": email,
		})
	}

	if err := h.fetchInitialData(client, userCacheFolder); err != nil {
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

	username := sess.Get("username")
	if username != nil {
		userStr, ok := username.(string)
		if ok {
			userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
			if err := h.clearUserCache(userCacheFolder); err != nil {
				fmt.Printf("Error clearing cache for user %s: %v\n", userStr, err)
			}
		}
	}

	if err := sess.Destroy(); err != nil {
		return c.Status(500).SendString("Error during logout")
	}

	return c.Redirect("/login")
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

func (h *AuthHandler) fetchInitialData(client *api.Client, cacheFolder string) error {
	folders, err := client.FetchFolders()
	if err != nil {
		return fmt.Errorf("failed to fetch folders: %v", err)
	}
	if err := utils.SaveCache(filepath.Join(cacheFolder, "folders.json"), folders); err != nil {
		return fmt.Errorf("failed to cache folders: %v", err)
	}

	messages, err := client.FetchMessages("INBOX", 10)
	if err != nil {
		return fmt.Errorf("failed to fetch messages: %v", err)
	}

	if err := utils.SaveCache(filepath.Join(cacheFolder, "emails.json"), messages); err != nil {
		return fmt.Errorf("failed to cache messages: %v", err)
	}

	return nil
}

// Add this method to the AuthHandler struct
func (h *AuthHandler) CreateIMAPClient(c *fiber.Ctx) (*api.Client, error) {
	// Get credentials from session
	sess, err := h.store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %v", err)
	}

	encryptedCreds := sess.Get("credentials")
	if encryptedCreds == nil {
		return nil, fmt.Errorf("no credentials found in session")
	}

	encryptedStr, ok := encryptedCreds.(string)
	if !ok {
		return nil, fmt.Errorf("invalid credentials format")
	}

	// Decrypt credentials
	creds, err := api.DecryptCredentials(encryptedStr, h.config.Encryption.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt credentials: %v", err)
	}

	// Get username from email
	var username string
	if h.config.Server.UsernameIsEmail {
		username = creds.Email
	} else {
		username = api.GetUsernameFromEmail(creds.Email)
	}

	if username == "" {
		return nil, fmt.Errorf("invalid email format")
	}

	// Create new IMAP client
	return api.NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		username,
		creds.Password,
	)
}

func (h *AuthHandler) CreateSMTPClient(c *fiber.Ctx) (*api.SMTPClient, error) {
	// Convert IMAP server to SMTP server (e.g., imap.gmail.com -> smtp.gmail.com)
	smtpServer := strings.Replace(h.config.IMAP.Server, "imap.", "smtp.", 1)

	// Get SMTP port from config, or use default
	smtpPort := h.config.SMTP.GetPort()

	// Get credentials from session
	sess, err := h.store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %v", err)
	}

	encryptedCreds := sess.Get("credentials")
	if encryptedCreds == nil {
		return nil, fmt.Errorf("no credentials found in session")
	}

	encryptedStr, ok := encryptedCreds.(string)
	if !ok {
		return nil, fmt.Errorf("invalid credentials format")
	}

	// Decrypt credentials
	creds, err := api.DecryptCredentials(encryptedStr, h.config.Encryption.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt credentials: %v", err)
	}

	client := api.NewSMTPClient(smtpServer, smtpPort, creds.Email, creds.Password)
	if client == nil {
		return nil, fmt.Errorf("failed to create SMTP client")
	}

	return client, nil
}
