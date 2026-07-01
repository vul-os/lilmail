// handlers/jsonapi/messages.go — JSON compose/send + draft-save endpoints.
//
// These mirror the HTMX compose path (handlers/web/email.go HandleComposeEmail /
// HandleSaveDraft) but accept a JSON body instead of a multipart form, and reuse
// the exact same engine: api.BuildMIMEMessage for assembly, SMTPClient for send,
// MailClient.SaveDraft / SaveToSent for IMAP persistence. No mail logic is
// duplicated — only the transport (JSON vs form) differs.
package jsonapi

import (
	"log"
	"path/filepath"
	"strings"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// composeBody is the JSON payload for POST /v1/messages and POST /v1/drafts.
// Attachments references previously-staged uploads (by token, from
// POST /v1/attachments) and/or inline base64 files; see compose_attachments.go.
type composeBody struct {
	To          string          `json:"to"`
	Cc          string          `json:"cc"`
	Bcc         string          `json:"bcc"`
	Subject     string          `json:"subject"`
	Text        string          `json:"text"`
	HTML        string          `json:"html"`
	InReplyTo   string          `json:"inReplyTo"`
	Attachments []attachmentRef `json:"attachments"`
}

// handleSend builds a MIME message from the JSON body and sends it over SMTP,
// then records recipients and best-effort saves a copy to the Sent folder.
// POST /v1/messages  body {to, cc?, bcc?, subject, text?, html?, inReplyTo?}
func (h *Handler) handleSend(c *fiber.Ctx) error {
	var body composeBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if strings.TrimSpace(body.To) == "" || strings.TrimSpace(body.Subject) == "" ||
		(body.Text == "" && body.HTML == "") {
		return fail(c, fiber.StatusBadRequest, "to, subject and a text or html body are required")
	}

	plain := body.Text
	if plain == "" && body.HTML != "" {
		plain = stripHTMLForPlain(body.HTML)
	}

	atts, err := h.resolveAttachments(c, body.Attachments)
	if err != nil {
		return failErr(c, err)
	}

	from := h.fromEmail(c)
	rawMessage, err := api.BuildMIMEMessage(api.MIMEMessageOptions{
		From:        from,
		To:          body.To,
		Cc:          body.Cc,
		Subject:     body.Subject,
		InReplyTo:   body.InReplyTo,
		PlainBody:   plain,
		HTMLBody:    body.HTML,
		Attachments: atts,
	})
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "failed to build message")
	}

	// Collect all RCPT TO addresses (To + Cc + Bcc).
	var allRcpts []string
	for _, a := range api.ParseAddressField(body.To) {
		allRcpts = append(allRcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(body.Cc) {
		allRcpts = append(allRcpts, a.Email)
	}
	for _, a := range api.ParseAddressField(body.Bcc) {
		allRcpts = append(allRcpts, a.Email)
	}

	smtpClient, err := h.smtpClient(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "failed to connect to mail server")
	}
	if err := smtpClient.SendRawMessage(allRcpts, rawMessage); err != nil {
		log.Printf("jsonapi: send: %v", err)
		return fail(c, fiber.StatusBadGateway, "failed to send message")
	}

	// Record recipients for autocomplete (best effort).
	if username, _ := c.Locals("username").(string); username != "" {
		dbPath := filepath.Join(h.config.Cache.Folder, api.SanitizeUsername(username), "threads.db")
		if rs, err := api.OpenRecipientsStore(dbPath); err == nil {
			defer rs.Close()
			var entries []api.RecipientEntry
			entries = append(entries, api.ParseAddressField(body.To)...)
			entries = append(entries, api.ParseAddressField(body.Cc)...)
			if err := rs.Record(entries); err != nil {
				log.Printf("jsonapi: record recipients: %v", err)
			}
		}
	}

	// Best-effort save to Sent.
	if cl, err := h.client(c); err == nil {
		defer cl.Close()
		if err := cl.SaveToSent(body.To, body.Subject, plain, rawMessage); err != nil {
			log.Printf("jsonapi: save to Sent: %v", err)
		}
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"sent": true})
}

// handleSaveDraft assembles a MIME message and appends it to the Drafts folder.
// POST /v1/drafts  body {to, cc?, subject, text?, html?, inReplyTo?}
func (h *Handler) handleSaveDraft(c *fiber.Ctx) error {
	var body composeBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if body.To == "" && body.Subject == "" && body.Text == "" && body.HTML == "" {
		return fail(c, fiber.StatusBadRequest, "draft is empty")
	}

	plain := body.Text
	if plain == "" && body.HTML != "" {
		plain = stripHTMLForPlain(body.HTML)
	}

	atts, err := h.resolveAttachments(c, body.Attachments)
	if err != nil {
		return failErr(c, err)
	}

	from := h.fromEmail(c)
	rawMessage, err := api.BuildMIMEMessage(api.MIMEMessageOptions{
		From:        from,
		To:          body.To,
		Cc:          body.Cc,
		Subject:     body.Subject,
		InReplyTo:   body.InReplyTo,
		PlainBody:   plain,
		HTMLBody:    body.HTML,
		Attachments: atts,
	})
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "failed to build draft")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "failed to connect to mail server")
	}
	defer cl.Close()

	if err := cl.SaveDraft(rawMessage); err != nil {
		log.Printf("jsonapi: save draft: %v", err)
		return fail(c, fiber.StatusBadGateway, "failed to save draft")
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"saved": true})
}

// handleMove moves a message to another folder (e.g. archive). The source
// folder comes from the `folder` query param (default INBOX), but an optional
// non-empty `folder` field in the JSON body overrides it.
// POST /v1/messages/:uid/move  ?folder=  body {toFolder, folder?}
func (h *Handler) handleMove(c *fiber.Ctx) error {
	uid := c.Params("uid")
	src := folderParam(c)

	var body struct {
		Folder   string `json:"folder"`
		ToFolder string `json:"toFolder"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if strings.TrimSpace(body.Folder) != "" {
		src = body.Folder
	}
	if strings.TrimSpace(body.ToFolder) == "" {
		return fail(c, fiber.StatusBadRequest, "toFolder is required")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	if err := cl.MoveMessage(src, uid, body.ToFolder); err != nil {
		log.Printf("jsonapi: move: %v", err)
		return fail(c, fiber.StatusBadGateway, "could not move message")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// stripHTMLForPlain produces a minimal plain-text fallback from an HTML body by
// dropping tags and collapsing whitespace. Used when only an HTML body is given.
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
	return strings.Join(strings.Fields(b.String()), " ")
}
