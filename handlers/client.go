package handlers

import (
	"fmt"
	"io"
	"lilmail/models"
	"strings"

	"github.com/emersion/go-imap"
)

// FetchMessages retrieves messages from a specified folder
func (c *Client) FetchMessages(folderName string, limit uint32) ([]models.Email, error) {
	// Select the mailbox (folder)
	mbox, err := c.client.Select(folderName, false) // false for read-only mode
	if err != nil {
		return nil, fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	// Check if mailbox is empty
	if mbox.Messages == 0 {
		return []models.Email{}, nil
	}

	// Determine the sequence numbers for the last N messages
	from := uint32(1)
	if mbox.Messages > limit {
		from = mbox.Messages - limit + 1
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, mbox.Messages)

	// Define the items we want to fetch
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchBody,
		imap.FetchBodyStructure,
		imap.FetchUid,
	}

	// Create a channel to receive the messages
	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)

	// Start the fetch operation
	go func() {
		done <- c.client.Fetch(seqSet, items, messages)
	}()

	var emails []models.Email

	// Process each message as it arrives
	for msg := range messages {
		email, err := c.processMessage(msg)
		if err != nil {
			// Log the error but continue processing other messages
			fmt.Printf("Error processing message %d: %v\n", msg.Uid, err)
			continue
		}
		emails = append(emails, email)
	}

	// Check if the fetch operation completed successfully
	if err := <-done; err != nil {
		return emails, fmt.Errorf("error during fetch: %v", err)
	}

	return emails, nil
}

// processMessage converts an IMAP message to our Email model
func (c *Client) processMessage(msg *imap.Message) (models.Email, error) {
	email := models.Email{
		ID:      fmt.Sprintf("%d", msg.Uid),
		Flags:   msg.Flags,
		Subject: msg.Envelope.Subject,
		Date:    msg.Envelope.Date,
	}

	// Process addresses
	if len(msg.Envelope.From) > 0 {
		email.From = msg.Envelope.From[0].Address()
		email.FromName = msg.Envelope.From[0].PersonalName
	}

	if len(msg.Envelope.To) > 0 {
		var toAddresses []string
		var toNames []string
		for _, addr := range msg.Envelope.To {
			toAddresses = append(toAddresses, addr.Address())
			toNames = append(toNames, addr.PersonalName)
		}
		email.To = strings.Join(toAddresses, ", ")
		email.ToNames = toNames
	}

	// Process CC recipients
	if len(msg.Envelope.Cc) > 0 {
		var ccAddresses []string
		for _, addr := range msg.Envelope.Cc {
			ccAddresses = append(ccAddresses, addr.Address())
		}
		email.Cc = strings.Join(ccAddresses, ", ")
	}

	// Get message bodies
	email.Body = c.getMessageBody(msg, false) // Plain text
	email.HTML = c.getMessageBody(msg, true)  // HTML

	// Process attachments
	attachments, err := c.processAttachments(msg)
	if err != nil {
		return email, fmt.Errorf("error processing attachments: %v", err)
	}
	email.Attachments = attachments

	return email, nil
}

// getMessageBody extracts either plain text or HTML body from the message
func (c *Client) getMessageBody(msg *imap.Message, wantHTML bool) string {
	if msg.BodyStructure == nil {
		return ""
	}

	var findSection func(bs *imap.BodyStructure, partNum []int) (string, bool)
	findSection = func(bs *imap.BodyStructure, partNum []int) (string, bool) {
		if bs == nil {
			return "", false
		}

		// Check if this is the part we want
		isDesiredPart := strings.ToLower(bs.MIMEType) == "text" &&
			((wantHTML && strings.ToLower(bs.MIMESubType) == "html") ||
				(!wantHTML && strings.ToLower(bs.MIMESubType) == "plain"))

		if isDesiredPart {
			// Create section string
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

		// Recursively check parts
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
			// Create section for attachment
			section := &imap.BodySectionName{}
			if len(partNum) > 0 {
				section.Specifier = imap.PartSpecifier(strings.Join(strings.Fields(fmt.Sprint(partNum)), "."))
			}

			// Get attachment content
			r := msg.GetBody(section)
			if r == nil {
				return fmt.Errorf("no body for attachment part %v", partNum)
			}

			content, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("error reading attachment content: %v", err)
			}

			// Create attachment
			attachment := models.Attachment{
				Filename:    bs.DispositionParams["filename"],
				ContentType: fmt.Sprintf("%s/%s", bs.MIMEType, bs.MIMESubType),
				Size:        len(content),
				Content:     content,
			}

			attachments = append(attachments, attachment)
		}

		// Recursively process parts
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
