// handlers/web/email.go
package web

import (
	"bytes"
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/models"
	"lilmail/utils"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// recipientsStorePath returns the path to the per-user bbolt database that
// stores both thread cache and recent recipients (shared file).
func recipientsStorePath(cacheFolder, username string) string {
	return filepath.Join(cacheFolder, api.SanitizeUsername(username), "threads.db")
}

// boltPath returns the path to the per-user bbolt thread-cache database.
func boltPath(cacheFolder, username string) string {
	return recipientsStorePath(cacheFolder, username)
}

type EmailHandler struct {
	store  *session.Store
	config *config.Config
	auth   *AuthHandler

	// threadStores caches one open bbolt handle per user so we don't open the
	// single-writer file on every request (which would cause lock contention).
	threadStoresMu sync.Mutex
	threadStores   map[string]*api.ThreadStore
}

func NewEmailHandler(store *session.Store, config *config.Config, auth *AuthHandler) *EmailHandler {
	return &EmailHandler{
		store:        store,
		config:       config,
		auth:         auth,
		threadStores: make(map[string]*api.ThreadStore),
	}
}

// getThreadStore returns the shared ThreadStore for the given user, opening it
// if necessary.  On failure it returns nil (callers fall back to in-memory).
func (h *EmailHandler) getThreadStore(username string) *api.ThreadStore {
	h.threadStoresMu.Lock()
	defer h.threadStoresMu.Unlock()

	if ts, ok := h.threadStores[username]; ok {
		return ts
	}
	path := boltPath(h.config.Cache.Folder, username)
	ts, err := api.OpenThreadStore(path)
	if err != nil {
		log.Printf("threadstore: open for %s: %v — will use in-memory threading", username, err)
		return nil
	}
	h.threadStores[username] = ts
	return ts
}

// buildThreads builds JWZ threads using the shared bbolt store when available,
// falling back to the package-level BuildThreads (which opens its own handle)
// on error.
func (h *EmailHandler) buildThreads(username, folder string, emails []models.Email) []models.Thread {
	ts := h.getThreadStore(username)
	if ts != nil {
		threads, err := ts.BuildThreads(folder, emails)
		if err == nil {
			return threads
		}
		log.Printf("threadstore: BuildThreads for %s/%s: %v — falling back", username, folder, err)
	}
	// Fallback: in-memory only (no bbolt persistence).
	return api.ThreadMessages(emails)
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
	userCacheFolder := filepath.Join(h.config.Cache.Folder, api.SanitizeUsername(userStr))
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

	// Build JWZ threads using the shared bbolt store.
	threads := h.buildThreads(userStr, "INBOX", emails)

	return c.Render("inbox", fiber.Map{
		"Username":      userStr,
		"Folders":       folders,
		"Emails":        emails,
		"Threads":       threads,
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
	userCacheFolder := filepath.Join(h.config.Cache.Folder, api.SanitizeUsername(userStr))
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

	// Build JWZ threads using the shared bbolt store.
	threads := h.buildThreads(userStr, folderName, emails)

	return c.Render("inbox", fiber.Map{
		"Username":      userStr,
		"Folders":       folders,
		"Emails":        emails,
		"Threads":       threads,
		"CurrentFolder": folderName,
		"Token":         token,
	})
}

// HandleEmailView handles the HTMX request for viewing a single email
func (h *EmailHandler) HandleEmailView(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
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
	// Detect Drafts folder so the template can show "Edit Draft" instead of Reply/Forward.
	isDrafts := strings.Contains(strings.ToLower(folderName), "draft")

	// Important: Set empty layout and only render the partial
	return c.Render("partials/email-viewer", fiber.Map{
		"Email":         email,
		"CurrentFolder": folderName,
		"IsDrafts":      isDrafts,
		"Layout":        "", // This is crucial to prevent full HTML rendering
	}, "") // Add empty string as second argument to explicitly disable layout
}

// HandleAttachment streams a single attachment to the browser. The attachment
// ID encodes the folder, message UID, and MIME part path; the content is
// fetched from the server on demand.
func (h *EmailHandler) HandleAttachment(c *fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(400).SendString("Attachment ID required")
	}

	folder, uid, part, err := api.DecodeAttachmentID(id)
	if err != nil {
		return c.Status(400).SendString("Invalid attachment ID")
	}

	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).SendString("Error connecting to email server")
	}
	defer client.Close()

	content, filename, contentType, err := client.FetchAttachment(folder, uid, part)
	if err != nil {
		log.Printf("Error fetching attachment %s/%s/%s: %v", folder, uid, part, err)
		return c.Status(500).SendString("Error fetching attachment")
	}

	// Enforce a 25 MiB limit on attachment downloads to avoid unbounded memory use.
	const maxAttachmentBytes = 25 * 1024 * 1024
	if len(content) > maxAttachmentBytes {
		return c.Status(413).SendString("Attachment exceeds maximum allowed size")
	}

	if contentType != "" {
		c.Set("Content-Type", contentType)
	}
	c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	return c.SendStream(bytes.NewReader(content), len(content))
}

// HandleDeleteEmail handles the email deletion request
func (h *EmailHandler) HandleDeleteEmail(c *fiber.Ctx) error {
	// Validate Authorization header
	token := c.Get("Authorization")
	if token == "" || len(token) < 8 || token[:7] != "Bearer " {
		return c.Status(401).SendString("Unauthorized")
	}

	// Validate JWT token
	_, err := api.ValidateToken(token[7:], h.config.JWT.Secret)
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

	// Delete the email
	err = client.DeleteMessage(folderName, emailID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"error": fmt.Sprintf("Error deleting email: %v", err),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Email deleted successfully",
	})
}

// handlers/web/email.go
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

	// Build JWZ threads using the shared bbolt store.
	userStr := ""
	if u, ok := username.(string); ok {
		userStr = u
	}
	threads := h.buildThreads(userStr, folderName, emails)

	return c.Render("partials/email-list", fiber.Map{
		"Emails":        emails,
		"Threads":       threads,
		"CurrentFolder": folderName,
		"Token":         token,
	}, "") // Explicitly set no layout
}

// HandleComposeEmail handles the email composition and sending.
// Supports:
//   - Plain-text and HTML (rich-text) bodies — multipart/alternative when both present
//   - File attachments — multipart/mixed wrapper with base64-encoded parts
//   - CC, BCC, In-Reply-To, References for reply/forward threading
//   - Draft deletion by UID when "draft_uid" form field is set (replaces draft on send)
//
// The form must use enctype="multipart/form-data" when attachments are included.
func (h *EmailHandler) HandleComposeEmail(c *fiber.Ctx) error {
	// Required fields.
	to := c.FormValue("to")
	subject := c.FormValue("subject")
	plainBody := c.FormValue("body")       // plain-text body
	htmlBody := c.FormValue("html_body")   // optional HTML body (rich-text editor)

	if to == "" || subject == "" || (plainBody == "" && htmlBody == "") {
		return c.Status(400).JSON(fiber.Map{
			"error": "To, subject and body are required",
		})
	}

	// If only HTML body is provided, generate a minimal plain-text version.
	if plainBody == "" && htmlBody != "" {
		plainBody = stripHTMLForPlain(htmlBody)
	}

	// Collect file attachments from the multipart form.
	var attachments []api.OutgoingAttachment
	form, _ := c.MultipartForm()
	if form != nil {
		for _, fhs := range form.File {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err != nil {
					log.Printf("compose: open attachment %q: %v", fh.Filename, err)
					continue
				}
				data := make([]byte, fh.Size)
				if _, err := f.Read(data); err != nil {
					f.Close()
					log.Printf("compose: read attachment %q: %v", fh.Filename, err)
					continue
				}
				f.Close()
				ct := fh.Header.Get("Content-Type")
				if ct == "" {
					ct = "application/octet-stream"
				}
				attachments = append(attachments, api.OutgoingAttachment{
					Filename:    fh.Filename,
					ContentType: ct,
					Data:        data,
				})
			}
		}
	}

	cc := c.FormValue("cc")
	bcc := c.FormValue("bcc")
	inReplyTo := c.FormValue("in_reply_to")
	references := c.FormValue("references")
	draftUID := c.FormValue("draft_uid") // UID of draft to delete after send

	// Get the sender email from the session.
	fromEmail := h.auth.GetSessionEmail(c)

	// Build the MIME message.
	mimeOpts := api.MIMEMessageOptions{
		From:        fromEmail,
		To:          to,
		Cc:          cc,
		Subject:     subject,
		InReplyTo:   inReplyTo,
		References:  references,
		PlainBody:   plainBody,
		HTMLBody:    htmlBody,
		Attachments: attachments,
	}
	rawMessage, err := api.BuildMIMEMessage(mimeOpts)
	if err != nil {
		log.Printf("compose: build MIME message: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": fmt.Sprintf("Failed to build message: %v", err),
		})
	}

	// Collect all RCPT TO addresses (To + CC + BCC).
	var allRcpts []string
	for _, a := range api.ParseAddressField(to) {
		allRcpts = append(allRcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(cc) {
		allRcpts = append(allRcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(bcc) {
		allRcpts = append(allRcpts, a.Email)
	}

	// Create SMTP client and send.
	smtpClient, err := h.auth.CreateSMTPClient(c)
	if err != nil {
		log.Printf("SMTP client creation error: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to connect to email server",
		})
	}

	if err = smtpClient.SendRawMessage(allRcpts, rawMessage); err != nil {
		log.Printf("Email sending error: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": fmt.Sprintf("Failed to send email: %v", err),
		})
	}

	// Record recipients for autocomplete.
	username, _ := c.Locals("username").(string)
	if username != "" {
		dbPath := recipientsStorePath(h.config.Cache.Folder, username)
		if rs, err := api.OpenRecipientsStore(dbPath); err == nil {
			defer rs.Close()
			var entries []api.RecipientEntry
			entries = append(entries, api.ParseAddressField(to)...)
			entries = append(entries, api.ParseAddressField(cc)...)
			if err := rs.Record(entries); err != nil {
				log.Printf("compose: record recipients: %v", err)
			}
		}
	}

	// Save to Sent folder (best effort).
	imapClient, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		log.Printf("IMAP client error when saving to Sent: %v", err)
	} else {
		defer imapClient.Close()
		if err := imapClient.SaveToSent(to, subject, plainBody, rawMessage); err != nil {
			log.Printf("Error saving to Sent folder: %v", err)
		}
		// If this was a draft, delete it from the Drafts folder.
		if draftUID != "" {
			if err := imapClient.DeleteDraft(draftUID); err != nil {
				log.Printf("compose: delete draft %s: %v", draftUID, err)
			}
		}
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Email sent successfully",
		"details": fiber.Map{
			"to":      to,
			"subject": subject,
		},
	})
}

// HandleSaveDraft saves or updates a draft message in the IMAP Drafts folder.
// Route: POST /api/draft
// Form fields: to, cc, bcc, subject, body, html_body, in_reply_to, references, draft_uid
// If draft_uid is set, the old draft is deleted before saving the new one.
func (h *EmailHandler) HandleSaveDraft(c *fiber.Ctx) error {
	to := c.FormValue("to")
	subject := c.FormValue("subject")
	plainBody := c.FormValue("body")
	htmlBody := c.FormValue("html_body")
	cc := c.FormValue("cc")
	inReplyTo := c.FormValue("in_reply_to")
	references := c.FormValue("references")
	oldUID := c.FormValue("draft_uid")

	if subject == "" && plainBody == "" && htmlBody == "" && to == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Draft is empty"})
	}

	if plainBody == "" && htmlBody != "" {
		plainBody = stripHTMLForPlain(htmlBody)
	}

	fromEmail := h.auth.GetSessionEmail(c)

	mimeOpts := api.MIMEMessageOptions{
		From:      fromEmail,
		To:        to,
		Cc:        cc,
		Subject:   subject,
		InReplyTo: inReplyTo,
		References: references,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
	}
	rawMessage, err := api.BuildMIMEMessage(mimeOpts)
	if err != nil {
		// If body is truly empty, build a minimal skeleton.
		rawMessage = []byte(fmt.Sprintf(
			"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n",
			fromEmail, to, subject,
		))
	}

	imapClient, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to connect to mail server"})
	}
	defer imapClient.Close()

	// Delete the previous version of the draft before saving the new one.
	if oldUID != "" {
		if err := imapClient.DeleteDraft(oldUID); err != nil {
			log.Printf("draft: delete old %s: %v", oldUID, err)
		}
	}

	if err := imapClient.SaveDraft(rawMessage); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Failed to save draft: %v", err)})
	}

	return c.JSON(fiber.Map{"success": true, "message": "Draft saved"})
}

// HandleListDrafts returns draft messages as an email-list partial.
// Route: GET /api/drafts
func (h *EmailHandler) HandleListDrafts(c *fiber.Ctx) error {
	username, _ := c.Locals("username").(string)

	token, err := api.GetSessionToken(c, h.store)
	if err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Invalid session"})
	}

	imapClient, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Failed to connect to mail server"})
	}
	defer imapClient.Close()

	draftsFolder, err := imapClient.DiscoverDraftsFolder()
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "No Drafts folder found"})
	}

	emails, err := imapClient.FetchMessages(draftsFolder, 50)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Failed to fetch drafts: %v", err)})
	}

	threads := h.buildThreads(username, draftsFolder, emails)

	return c.Render("partials/email-list", fiber.Map{
		"Emails":        emails,
		"Threads":       threads,
		"CurrentFolder": draftsFolder,
		"IsDrafts":      true,
		"Token":         token,
	}, "")
}

// HandleAutocomplete returns recipient suggestions for the compose modal.
// Route: GET /api/autocomplete?q=<query>
// Returns JSON array of {email, name} objects.
func (h *EmailHandler) HandleAutocomplete(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	username, _ := c.Locals("username").(string)

	const limit = 10

	// Recent recipients from bbolt.
	var results []api.RecipientEntry
	if username != "" {
		dbPath := recipientsStorePath(h.config.Cache.Folder, username)
		if rs, err := api.OpenRecipientsStore(dbPath); err == nil {
			defer rs.Close()
			if res, err := rs.Search(query, limit); err == nil {
				results = res
			}
		}
	}

	// CardDAV contacts (if configured and we haven't hit the limit).
	if len(results) < limit && h.config.CardDAV.Enabled {
		remaining := limit - len(results)
		cardContacts := api.CardDAVContacts(
			h.config.CardDAV.URL,
			h.config.CardDAV.Username,
			h.config.CardDAV.Password,
			query,
			remaining,
		)
		// Deduplicate: skip addresses already in results.
		seen := make(map[string]bool)
		for _, r := range results {
			seen[strings.ToLower(r.Email)] = true
		}
		for _, r := range cardContacts {
			if !seen[strings.ToLower(r.Email)] {
				results = append(results, r)
				seen[strings.ToLower(r.Email)] = true
			}
		}
	}

	// Return simple JSON array.
	type suggestion struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	out := make([]suggestion, 0, len(results))
	for _, r := range results {
		out = append(out, suggestion{Email: r.Email, Name: r.Name})
	}
	return c.JSON(out)
}

// stripHTMLForPlain produces a minimal plain-text version of an HTML string by
// stripping tags and collapsing whitespace. Used to auto-generate the
// text/plain alternative when only HTML body is provided.
func stripHTMLForPlain(html string) string {
	var b strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// Collapse runs of whitespace.
	return strings.Join(strings.Fields(b.String()), " ")
}

// HandleMarkUnread removes the \Seen flag from a message, marking it as unread.
// Route: PATCH /api/email/:id/unread
// Requires Authorization: Bearer <jwt> and X-Folder (or ?folder=) query param.
func (h *EmailHandler) HandleMarkUnread(c *fiber.Ctx) error {
	emailID := c.Params("id")
	if emailID == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Email ID required"})
	}

	folderName := c.Get("X-Folder")
	if folderName == "" {
		folderName = c.Query("folder")
		if folderName == "" {
			folderName = "INBOX"
		}
	}

	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Error connecting to email server"})
	}
	defer client.Close()

	// Remove \Seen flag to mark the message as unread.
	if err := client.SetMessageFlag(folderName, emailID, `\Seen`, false); err != nil {
		log.Printf("HandleMarkUnread: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Failed to mark unread: %v", err)})
	}

	return c.JSON(fiber.Map{"success": true, "message": "Marked as unread"})
}

// HandleSearch performs an IMAP SEARCH and returns matching messages as an
// email-list partial.
// Route: GET /api/search?q=<query>&folder=<folder>
func (h *EmailHandler) HandleSearch(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		return c.Status(400).JSON(fiber.Map{"error": "q parameter required"})
	}

	folderName := c.Query("folder")
	if folderName == "" {
		folderName = "INBOX"
	}

	username := c.Locals("username")
	userStr := ""
	if u, ok := username.(string); ok {
		userStr = u
	}

	token, err := api.GetSessionToken(c, h.store)
	if err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Invalid session"})
	}

	client, err := h.auth.CreateIMAPClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Error connecting to email server"})
	}
	defer client.Close()

	emails, err := client.SearchMessages(folderName, query, 50)
	if err != nil {
		log.Printf("HandleSearch: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Search failed: %v", err)})
	}

	threads := h.buildThreads(userStr, folderName+":search:"+query, emails)

	return c.Render("partials/email-list", fiber.Map{
		"Emails":        emails,
		"Threads":       threads,
		"CurrentFolder": folderName,
		"Token":         token,
		"SearchQuery":   query,
	}, "")
}
