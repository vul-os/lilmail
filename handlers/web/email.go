// handlers/web/email.go
package web

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/utils"
	"log"
	"net/url"
	"path/filepath"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

type EmailHandler struct {
	store  *session.Store
	config *config.Config
	auth   *AuthHandler
}

func NewEmailHandler(store *session.Store, config *config.Config, auth *AuthHandler) *EmailHandler {
	return &EmailHandler{
		store:  store,
		config: config,
		auth:   auth,
	}
}

// HandleInbox renders the main inbox page
func (h *EmailHandler) HandleInbox(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Redirect("/login")
	}

	userStr, ok := username.(string)
	if !ok {
		return c.Redirect("/login")
	}

	// Load folders from cache
	userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
	var folders []*api.MailboxInfo
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "folders.json"), &folders); err != nil {
		return c.Status(500).SendString("Error loading folders")
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	// Fetch inbox messages
	emails, err := client.FetchMessages("INBOX", 50)
	if err != nil {
		return c.Status(500).SendString("Error fetching emails")
	}

	// Get JWT token for API requests
	token, err := api.GetSessionToken(c, h.store)
	if err != nil {
		return c.Redirect("/login")
	}

	return c.Render("inbox", fiber.Map{
		"Username":      userStr,
		"Folders":       folders,
		"Emails":        emails,
		"CurrentFolder": "INBOX",
		"Token":         token,
	})
}

// HandleFolder displays emails from a specific folder
func (h *EmailHandler) HandleFolder(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Redirect("/login")
	}

	userStr, ok := username.(string)
	if !ok {
		return c.Redirect("/login")
	}

	folderName, err := url.QueryUnescape(c.Params("name"))
	if folderName == "" {
		return c.Redirect("/inbox")
	}

	// Load folders for sidebar
	userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
	var folders []*api.MailboxInfo
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "folders.json"), &folders); err != nil {
		return c.Status(500).SendString("Error loading folders")
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	// Fetch folder emails
	emails, err := client.FetchMessages(folderName, 50)
	if err != nil {
		return c.Status(500).SendString("Error fetching emails")
	}

	// Get JWT token for API requests
	token, err := api.GetSessionToken(c, h.store)
	if err != nil {
		return c.Redirect("/login")
	}

	return c.Render("inbox", fiber.Map{
		"Username":      userStr,
		"Folders":       folders,
		"Emails":        emails,
		"CurrentFolder": folderName,
		"Token":         token,
	})
}

// HandleEmailView handles the template rendering for viewing a single email
func (h *EmailHandler) HandleEmailView(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
	}

	// Validate JWT token
	claims, err := api.ValidateToken(token[7:], h.config.JWT.Secret)
	if err != nil {
		return c.Status(401).SendString("Invalid token")
	}

	// Get folder and email ID
	folderName := c.Get("X-Folder")
	if folderName == "" {
		folderName = c.Query("folder")
		if folderName == "" {
			folderName = "INBOX"
		}
	}

	emailID := c.Params("id")
	if emailID == "" {
		return c.Status(400).SendString("Email ID required")
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": "Error connecting to email server",
		})
	}
	defer client.Close()

	// Fetch the email
	email, err := client.FetchSingleMessage(folderName, emailID)
	if err != nil {
		log.Printf("Error fetching email %s from folder %s: %v", emailID, folderName, err)
		return c.Status(500).JSON(fiber.Map{
			"error": fmt.Sprintf("Error fetching email: %v", err),
		})
	}

	// Cache the email
	userCacheFolder := filepath.Join(h.config.Cache.Folder, claims.Username)
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("email_%s.json", emailID)), email); err != nil {
		log.Printf("Error caching email %s: %v\n", emailID, err)
	}

	return c.Render("partials/email-viewer", fiber.Map{
		"Email":  email,
		"Layout": "",
	})
}

// HandleFolderEmails handles template rendering for folder contents
func (h *EmailHandler) HandleFolderEmails(c *fiber.Ctx) error {
	folderName, err := url.QueryUnescape(c.Params("name"))
	if err != nil || folderName == "" {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid folder name",
		})
	}

	username := c.Locals("username")
	if username == nil {
		return c.Status(401).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get JWT token for API requests
	token, err := api.GetSessionToken(c, h.store)
	if err != nil {
		return c.Status(401).JSON(fiber.Map{
			"error": "Invalid session",
		})
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": "Error connecting to email server",
		})
	}
	defer client.Close()

	// Fetch emails from the folder
	emails, err := client.FetchMessages(folderName, 50)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": fmt.Sprintf("Error fetching emails: %v", err),
		})
	}

	return c.Render("partials/email-list", fiber.Map{
		"Emails":        emails,
		"CurrentFolder": folderName,
		"Token":         token,
		"Layout":        "",
	})
}
