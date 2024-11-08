package handlers

import (
	"fmt"
	"lilmail/config"
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

type MailboxInfo struct {
	Attributes  []string `json:"attributes"`
	Delimiter   string   `json:"delimiter"`
	Name        string   `json:"name"`
	UnreadCount int      `json:"unreadCount,omitempty"`
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
	// Get username from context (set by middleware)
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
	var folders []*MailboxInfo
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
	token, err := h.auth.GetSessionToken(c)
	if err != nil {
		return c.Redirect("/login")
	}

	// Render inbox template
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

	// Decode the folder name from URL
	folderName, err := url.QueryUnescape(c.Params("name"))
	if folderName == "" {
		return c.Redirect("/inbox")
	}

	// Load folders for sidebar
	userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
	var folders []*MailboxInfo
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
	token, err := h.auth.GetSessionToken(c)
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

// HandleEmailView handles the AJAX request for viewing a single email
func (h *EmailHandler) HandleEmailView(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
	}

	// Validate JWT token
	claims, err := h.auth.ValidateToken(token[7:])
	if err != nil {
		return c.Status(401).SendString("Invalid token")
	}

	emailID := c.Params("id")
	if emailID == "" {
		return c.Status(400).SendString("Email ID required")
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	// Fetch the email
	email, err := client.FetchSingleMessage(emailID)
	if err != nil {
		return c.Status(500).SendString("Error fetching email")
	}

	// Cache the email for future reference
	userCacheFolder := filepath.Join(h.config.Cache.Folder, claims.Username)
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("email_%s.json", emailID)), email); err != nil {
		// Log the error but don't fail the request
		log.Printf("Error caching email %s: %v\n", emailID, err)
	}

	// Render the email view partial template
	return c.Render("partials/email-viewer", fiber.Map{
		"Email":  email,
		"Layout": "", // Ensure no layout is used
	})
}

// HandleFolderEmails handles AJAX requests for folder contents
func (h *EmailHandler) HandleFolderEmails(c *fiber.Ctx) error {
	// Get folder name from params and decode it
	folderName, err := url.QueryUnescape(c.Params("name"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "Invalid folder name",
		})
	}

	if folderName == "" {
		return c.Status(400).JSON(fiber.Map{
			"error": "Folder name required",
		})
	}

	// Get username from context
	username := c.Locals("username")
	if username == nil {
		return c.Status(401).JSON(fiber.Map{
			"error": "Unauthorized",
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

	// Cache the emails
	userCacheFolder := filepath.Join(h.config.Cache.Folder, username.(string))
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("folder_%s.json", folderName)), emails); err != nil {
		log.Printf("Error caching folder %s: %v\n", folderName, err)
	}

	// Render the email list partial
	return c.Render("partials/email-list", fiber.Map{
		"Emails": emails,
		"Layout": "", // No layout for partial
	})
}

// HandleComposeEmail handles the compose email form submission
func (h *EmailHandler) HandleComposeEmail(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
	}

	// Validate JWT token
	_, err := h.auth.ValidateToken(token[7:])
	if err != nil {
		return c.Status(401).SendString("Invalid token")
	}

	// Get form values
	to := c.FormValue("to")
	subject := c.FormValue("subject")
	body := c.FormValue("body")

	if to == "" || subject == "" || body == "" {
		return c.Status(400).SendString("All fields are required")
	}

	// // Get SMTP client (you'll need to implement this)
	// client, err := h.auth.CreateSMTPClient(c)
	// if err != nil {
	// 	return c.Status(500).SendString("Error connecting to email server")
	// }
	// defer client.Close()

	// Send the email
	// err = client.SendEmail(to, subject, body)
	// if err != nil {
	// 	return c.Status(500).SendString("Error sending email")
	// }

	return c.SendString("Email sent successfully")
}

// HandleDeleteEmail handles email deletion
func (h *EmailHandler) HandleDeleteEmail(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
	}

	// Validate JWT token
	_, err := h.auth.ValidateToken(token[7:])
	if err != nil {
		return c.Status(401).SendString("Invalid token")
	}

	emailID := c.Params("id")
	if emailID == "" {
		return c.Status(400).SendString("Email ID required")
	}

	// Get IMAP client
	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	// // Delete the email
	// err = client.DeleteMessage(emailID)
	// if err != nil {
	// 	return c.Status(500).SendString("Error deleting email")
	// }

	return c.SendString("Email deleted successfully")
}
