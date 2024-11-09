// handlers/api/email.go
package api

import (
	"fmt"
	"io"
	"lilmail/models"
	"strings"

	"github.com/emersion/go-imap"
)

// FetchMessages retrieves messages from a specified folder
func (c *Client) FetchMessages(folderName string, limit uint32) ([]models.Email, error) {
	mbox, err := c.client.Select(folderName, false)
	if err != nil {
		return nil, fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	if mbox.Messages == 0 {
		return []models.Email{}, nil
	}

	from := uint32(1)
	if mbox.Messages > limit {
		from = mbox.Messages - limit + 1
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, mbox.Messages)

	messages := make(chan *imap.Message, limit)
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchBody,
		imap.FetchBodyStructure,
		imap.FetchUid,
	}

	done := make(chan error, 1)
	go func() {
		done <- c.client.Fetch(seqSet, items, messages)
	}()

	var emails []models.Email
	for msg := range messages {
		email, err := c.processMessage(msg)
		if err != nil {
			fmt.Printf("Error processing message %d: %v\n", msg.Uid, err)
			continue
		}
		emails = append(emails, email)
	}

	if err := <-done; err != nil {
		return emails, fmt.Errorf("error during fetch: %v", err)
	}

	return emails, nil
}

// FetchSingleMessage retrieves a specific message by its UID
func (c *Client) FetchSingleMessage(folderName, uid string) (models.Email, error) {
	uidNum, err := parseUID(uid)
	if err != nil {
		return models.Email{}, fmt.Errorf("invalid UID: %v", err)
	}

	_, err = c.client.Select(folderName, true)
	if err != nil {
		return models.Email{}, fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	messages := make(chan *imap.Message, 1)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchBodyStructure,
		imap.FetchUid,
		section.FetchItem(),
	}

	done := make(chan error, 1)
	go func() {
		done <- c.client.UidFetch(seqSet, items, messages)
	}()

	var email models.Email
	found := false

	for msg := range messages {
		if msg != nil {
			email, err = c.processMessage(msg)
			if err != nil {
				return models.Email{}, fmt.Errorf("error processing message: %v", err)
			}
			found = true
			break
		}
	}

	if err := <-done; err != nil {
		return models.Email{}, fmt.Errorf("error during fetch: %v", err)
	}

	if !found {
		return models.Email{}, fmt.Errorf("message with UID %s not found in folder %s", uid, folderName)
	}

	return email, nil
}

// DeleteMessage deletes a specific message by its UID
func (c *Client) DeleteMessage(folderName, uid string) error {
	uidNum, err := parseUID(uid)
	if err != nil {
		return fmt.Errorf("invalid UID: %v", err)
	}

	_, err = c.client.Select(folderName, false)
	if err != nil {
		return fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	// Mark as deleted
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}

	err = c.client.UidStore(seqSet, item, flags, nil)
	if err != nil {
		return fmt.Errorf("error marking message as deleted: %v", err)
	}

	// Expunge to permanently remove
	err = c.client.Expunge(nil)
	if err != nil {
		return fmt.Errorf("error expunging mailbox: %v", err)
	}

	return nil
}

// MarkMessageAsRead marks a message as read
func (c *Client) MarkMessageAsRead(folderName, uid string) error {
	return c.setMessageFlag(folderName, uid, imap.SeenFlag, true)
}

// MarkMessageAsUnread marks a message as unread
func (c *Client) MarkMessageAsUnread(folderName, uid string) error {
	return c.setMessageFlag(folderName, uid, imap.SeenFlag, false)
}

// setMessageFlag is a helper function to set or remove flags
func (c *Client) setMessageFlag(folderName, uid string, flag string, add bool) error {
	uidNum, err := parseUID(uid)
	if err != nil {
		return fmt.Errorf("invalid UID: %v", err)
	}

	_, err = c.client.Select(folderName, false)
	if err != nil {
		return fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	var operation imap.FlagsOp
	if add {
		operation = imap.AddFlags
	} else {
		operation = imap.RemoveFlags
	}

	item := imap.FormatFlagsOp(operation, true)
	flags := []interface{}{flag}

	err = c.client.UidStore(seqSet, item, flags, nil)
	if err != nil {
		return fmt.Errorf("error setting message flag: %v", err)
	}

	return nil
}

// processAttachments extracts attachments from the message
func (c *Client) processAttachments(msg *imap.Message) ([]models.Attachment, error) {
	var attachments []models.Attachment

	var processAttachmentPart func(bs *imap.BodyStructure, partNum []int) error
	processAttachmentPart = func(bs *imap.BodyStructure, partNum []int) error {
		if bs == nil {
			return nil
		}

		isAttachment := bs.Disposition == "attachment" ||
			(bs.Disposition == "inline" && bs.MIMEType != "text")

		if isAttachment {
			section := &imap.BodySectionName{}
			if len(partNum) > 0 {
				section.Specifier = imap.PartSpecifier(strings.Join(strings.Fields(fmt.Sprint(partNum)), "."))
			}

			r := msg.GetBody(section)
			if r == nil {
				return fmt.Errorf("no body for attachment part %v", partNum)
			}

			content, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("error reading attachment content: %v", err)
			}

			attachment := models.Attachment{
				Filename:    bs.DispositionParams["filename"],
				ContentType: fmt.Sprintf("%s/%s", bs.MIMEType, bs.MIMESubType),
				Size:        len(content),
				Content:     content,
			}

			attachments = append(attachments, attachment)
		}

		for i, part := range bs.Parts {
			newPartNum := append(partNum, i+1)
			if err := processAttachmentPart(part, newPartNum); err != nil {
				return err
			}
		}

		return nil
	}

	err := processAttachmentPart(msg.BodyStructure, nil)
	return attachments, err
}

// processMessage converts an IMAP message to our Email model
func (c *Client) processMessage(msg *imap.Message) (models.Email, error) {
	email := models.Email{
		ID:    fmt.Sprintf("%d", msg.Uid),
		Flags: msg.Flags,
	}

	if msg.Envelope != nil {
		email.Subject = msg.Envelope.Subject
		email.Date = msg.Envelope.Date

		if len(msg.Envelope.From) > 0 {
			email.From = msg.Envelope.From[0].Address()
			email.FromName = msg.Envelope.From[0].PersonalName
		}

		if len(msg.Envelope.To) > 0 {
			var toAddresses []string
			for _, addr := range msg.Envelope.To {
				toAddresses = append(toAddresses, addr.Address())
			}
			email.To = strings.Join(toAddresses, ", ")
		}

		if len(msg.Envelope.Cc) > 0 {
			var ccAddresses []string
			for _, addr := range msg.Envelope.Cc {
				ccAddresses = append(ccAddresses, addr.Address())
			}
			email.Cc = strings.Join(ccAddresses, ", ")
		}
	}

	// Get message body (try HTML first, fall back to plain text)
	body := c.getMessageBody(msg, true) // Try HTML first
	if body != "" {
		email.HTML = body
		email.IsHTML = true
	} else {
		body = c.getMessageBody(msg, false) // Fall back to plain text
		// Clean up the plain text body
		body = strings.TrimSpace(body)
		// Remove common email headers that might appear in the body
		lines := strings.Split(body, "\n")
		var cleanedLines []string
		for _, line := range lines {
			if !strings.HasPrefix(line, "From:") &&
				!strings.HasPrefix(line, "To:") &&
				!strings.HasPrefix(line, "Subject:") &&
				!strings.HasPrefix(line, "Date:") &&
				!strings.HasPrefix(line, "Message-ID:") &&
				!strings.HasPrefix(line, "MIME-Version:") &&
				!strings.HasPrefix(line, "Content-Type:") &&
				!strings.HasPrefix(line, "Content-Transfer-Encoding:") {
				cleanedLines = append(cleanedLines, line)
			}
		}
		email.Body = strings.Join(cleanedLines, "\n")
	}

	// Set preview from plain text
	plainText := c.getMessageBody(msg, false)
	if plainText != "" {
		// Clean up the preview text
		preview := strings.TrimSpace(plainText)
		preview = strings.Join(strings.Fields(preview), " ") // Normalize whitespace
		if len(preview) > 150 {
			preview = preview[:150] + "..."
		}
		email.Preview = preview
	}

	// Process attachments if any
	attachments, err := c.processAttachments(msg)
	if err != nil {
		return email, fmt.Errorf("error processing attachments: %v", err)
	}
	email.Attachments = attachments
	email.HasAttachments = len(attachments) > 0

	return email, nil
}

// Clean up the getMessageBody method as well
func (c *Client) getMessageBody(msg *imap.Message, wantHTML bool) string {
	if msg.BodyStructure == nil {
		return ""
	}

	var findSection func(bs *imap.BodyStructure, partNum []int) (string, bool)
	findSection = func(bs *imap.BodyStructure, partNum []int) (string, bool) {
		if bs == nil {
			return "", false
		}

		isDesiredPart := strings.ToLower(bs.MIMEType) == "text" &&
			((wantHTML && strings.ToLower(bs.MIMESubType) == "html") ||
				(!wantHTML && strings.ToLower(bs.MIMESubType) == "plain"))

		if isDesiredPart {
			section := &imap.BodySectionName{}
			if len(partNum) > 0 {
				section.Specifier = imap.PartSpecifier(strings.Join(strings.Fields(fmt.Sprint(partNum)), "."))
			}

			r := msg.GetBody(section)
			if r == nil {
				return "", false
			}

			body, err := io.ReadAll(r)
			if err != nil {
				return "", false
			}

			return string(body), true
		}

		for i, part := range bs.Parts {
			newPartNum := append(partNum, i+1)
			if body, found := findSection(part, newPartNum); found {
				return body, true
			}
		}

		return "", false
	}

	body, _ := findSection(msg.BodyStructure, nil)
	return body
}
