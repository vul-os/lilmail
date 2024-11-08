// handlers/email.go

package handlers

import (
	"fmt"
	"lilmail/config"
	"lilmail/models"
	"lilmail/utils"
	"path/filepath"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

type EmailHandler struct {
	store  *session.Store
	config *config.Config
}

func NewEmailHandler(store *session.Store, config *config.Config) *EmailHandler {
	return &EmailHandler{
		store:  store,
		config: config,
	}
}

// HandleInbox renders the inbox page
func (h *EmailHandler) HandleInbox(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Redirect("/login")
	}

	usernameStr, ok := username.(string)
	if !ok {
		return c.Redirect("/login")
	}

	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)

	// Load folders
	var folders []*MailboxInfo
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "folders.json"), &folders); err != nil {
		return c.Status(500).SendString("Failed to load folders")
	}

	// Load emails
	var emails []models.Email
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "emails.json"), &emails); err != nil {
		return c.Status(500).SendString("Failed to load emails")
	}

	return c.Render("inbox", fiber.Map{
		"Username":      usernameStr,
		"Folders":       folders,
		"Emails":        emails,
		"CurrentFolder": "INBOX",
	})
}

// HandleFolder handles displaying emails from a specific folder
func (h *EmailHandler) HandleFolder(c *fiber.Ctx) error {
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

	// Get IMAP client
	client, err := h.getIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	// Load folders for sidebar
	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)
	var folders []*MailboxInfo
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "folders.json"), &folders); err != nil {
		return c.Status(500).SendString("Error loading folders")
	}

	// Fetch folder emails
	emails, err := client.FetchMessages(folderName, 10)
	if err != nil {
		return c.Status(500).SendString("Error fetching emails")
	}

	// Cache the emails for this folder
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("%s.json", folderName)), emails); err != nil {
		// Log error but don't fail the request
		fmt.Printf("Error caching emails for folder %s: %v\n", folderName, err)
	}

	return c.Render("inbox", fiber.Map{
		"Username":      usernameStr,
		"Folders":       folders,
		"Emails":        emails,
		"CurrentFolder": folderName,
	})
}

// HandleSingleEmail displays a single email
func (h *EmailHandler) HandleSingleEmail(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Redirect("/login")
	}

	usernameStr, ok := username.(string)
	if !ok {
		return c.Redirect("/login")
	}

	emailID := c.Params("id")
	if emailID == "" {
		return c.Redirect("/inbox")
	}

	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)

	// Load current folder's emails
	var emails []models.Email
	if err := utils.LoadCache(filepath.Join(userCacheFolder, "emails.json"), &emails); err != nil {
		return c.Status(500).SendString("Failed to load emails")
	}

	// Find the requested email
	var targetEmail models.Email
	found := false
	for _, email := range emails {
		if email.ID == emailID {
			targetEmail = email
			found = true
			break
		}
	}

	if !found {
		return c.Status(404).SendString("Email not found")
	}

	return c.Render("email", fiber.Map{
		"Username": usernameStr,
		"Email":    targetEmail,
	})
}

// HandleFolderEmails handles AJAX requests for folder emails
func (h *EmailHandler) HandleFolderEmails(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Status(401).JSON(fiber.Map{"error": "Unauthorized"})
	}

	usernameStr, ok := username.(string)
	if !ok {
		return c.Status(401).JSON(fiber.Map{"error": "Invalid session"})
	}

	folderName := c.Params("name")
	if folderName == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Folder name required"})
	}

	// Get IMAP client
	client, err := h.getIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Error connecting to email server"})
	}
	defer client.Close()

	// Fetch emails from folder
	emails, err := client.FetchMessages(folderName, 10)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Error fetching emails: %v", err)})
	}

	// Cache the emails
	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("%s.json", folderName)), emails); err != nil {
		fmt.Printf("Error caching emails for folder %s: %v\n", folderName, err)
	}

	return c.JSON(fiber.Map{
		"emails": emails,
	})
}

// HandleRefreshEmails handles email refresh requests
func (h *EmailHandler) HandleRefreshEmails(c *fiber.Ctx) error {
	username := c.Locals("username")
	if username == nil {
		return c.Status(401).JSON(fiber.Map{"error": "Unauthorized"})
	}

	usernameStr, ok := username.(string)
	if !ok {
		return c.Status(401).JSON(fiber.Map{"error": "Invalid session"})
	}

	folderName := c.Query("folder", "INBOX")

	// Get IMAP client
	client, err := h.getIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Error connecting to email server"})
	}
	defer client.Close()

	// Fetch fresh emails
	emails, err := client.FetchMessages(folderName, 10)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Error fetching emails: %v", err)})
	}

	// Update cache
	userCacheFolder := filepath.Join(h.config.Cache.Folder, usernameStr)
	if err := utils.SaveCache(filepath.Join(userCacheFolder, fmt.Sprintf("%s.json", folderName)), emails); err != nil {
		fmt.Printf("Error caching emails for folder %s: %v\n", folderName, err)
	}

	return c.JSON(fiber.Map{
		"emails": emails,
	})
}

// Helper function to get IMAP client
func (h *EmailHandler) getIMAPClient(c *fiber.Ctx) (*Client, error) {
	sess, err := h.store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("session error: %v", err)
	}

	email := sess.Get("email")
	if email == nil {
		return nil, fmt.Errorf("email not found in session")
	}

	emailStr, ok := email.(string)
	if !ok {
		return nil, fmt.Errorf("invalid email in session")
	}

	// You should implement a secure way to handle passwords
	password := sess.Get("password")
	if password == nil {
		return nil, fmt.Errorf("password not found in session")
	}

	passwordStr, ok := password.(string)
	if !ok {
		return nil, fmt.Errorf("invalid password in session")
	}

	return NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		emailStr,
		passwordStr,
	)
}

type MailboxInfo struct {
	Name       string   `json:"name"`
	Delimiter  string   `json:"delimiter"`
	Attributes []string `json:"attributes"`
}
