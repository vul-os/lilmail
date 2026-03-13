// handlers/api/email.go
package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"lilmail/models"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/emersion/go-imap"
)

func partPathID(partNum []int) string {
	if len(partNum) == 0 {
		return ""
	}
	parts := make([]string, len(partNum))
	for i, n := range partNum {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

func decodeTransferEncoding(content []byte, transferEncoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		decoded := make([]byte, base64.StdEncoding.DecodedLen(len(content)))
		n, err := base64.StdEncoding.Decode(decoded, bytes.TrimSpace(content))
		if err != nil {
			cleaned := bytes.Map(func(r rune) rune {
				switch r {
				case '\r', '\n', '\t', ' ':
					return -1
				default:
					return r
				}
			}, content)
			n, err = base64.StdEncoding.Decode(decoded, cleaned)
			if err != nil {
				return content
			}
		}
		return decoded[:n]
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(content)))
		if err != nil {
			return content
		}
		return decoded
	default:
		return content
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func fallbackString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func isAttachmentContent(disposition, lowerMediaType, filename string) bool {
	return strings.EqualFold(disposition, "attachment") ||
		(strings.EqualFold(disposition, "inline") && !strings.HasPrefix(lowerMediaType, "text/")) ||
		(filename != "" && !strings.HasPrefix(lowerMediaType, "text/"))
}

func buildTextSection(partNum []int) *imap.BodySectionName {
	section := &imap.BodySectionName{Peek: true}
	section.Specifier = imap.TextSpecifier
	section.Path = append([]int(nil), partNum...)
	return section
}

func childPartPath(partNum []int, index int) []int {
	return append(append([]int{}, partNum...), index+1)
}

func parseMessagePart(header textproto.MIMEHeader, partData []byte, email *models.Email, attachments *[]models.Attachment, attachmentCounter *int) {
	contentType := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(contentType))
	}

	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}

		mr := multipart.NewReader(bytes.NewReader(partData), boundary)
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}

			nestedData, err := ioutil.ReadAll(p)
			if err != nil {
				p.Close()
				continue
			}

			parseMessagePart(p.Header, nestedData, email, attachments, attachmentCounter)
			p.Close()
		}
		return
	}

	lowerMediaType := strings.ToLower(mediaType)
	decodedPartData := decodeTransferEncoding(partData, header.Get("Content-Transfer-Encoding"))
	if strings.HasPrefix(lowerMediaType, "text/plain") && email.Body == "" {
		email.Body = string(decodedPartData)
	}
	if strings.HasPrefix(lowerMediaType, "text/html") && email.HTML == "" {
		email.HTML = template.HTML(decodedPartData)
	}

	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := firstNonEmpty(dispParams["filename"], params["name"])
	isAttachment := isAttachmentContent(disposition, lowerMediaType, filename)

	if isAttachment {
		filename = fallbackString(filename, fmt.Sprintf("attachment-%d", *attachmentCounter))
		*attachments = append(*attachments, models.Attachment{
			ID:          strconv.Itoa(*attachmentCounter),
			Filename:    filename,
			ContentType: mediaType,
			Size:        len(decodedPartData),
			Content:     decodedPartData,
		})
		*attachmentCounter++
	}
}

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

func (c *Client) FetchSingleMessage(folderName, uid string) (models.Email, error) {
	uidNum, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return models.Email{}, fmt.Errorf("invalid UID: %v", err)
	}

	// Select the folder
	_, err = c.client.Select(folderName, true)
	if err != nil {
		return models.Email{}, fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uint32(uidNum))

	// Define the items to fetch
	section := &imap.BodySectionName{
		Peek: true,
	}

	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchBodyStructure,
		imap.FetchUid,
		section.FetchItem(),
	}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.client.UidFetch(seqSet, items, messages)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
		break
	}

	if err := <-done; err != nil {
		return models.Email{}, fmt.Errorf("fetch error: %v", err)
	}

	if msg == nil {
		return models.Email{}, fmt.Errorf("message not found")
	}

	return c.processMessage(msg)
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

		lowerMediaType := strings.ToLower(fmt.Sprintf("%s/%s", bs.MIMEType, bs.MIMESubType))
		filename := firstNonEmpty(bs.DispositionParams["filename"], bs.Params["name"])
		isAttachment := isAttachmentContent(bs.Disposition, lowerMediaType, filename)

		if isAttachment {
			section := buildTextSection(partNum)

			r := msg.GetBody(section)
			if r == nil {
				return fmt.Errorf("no body for attachment part %v", partNum)
			}

			content, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("error reading attachment content: %v", err)
			}
			content = decodeTransferEncoding(content, bs.Encoding)

			attachment := models.Attachment{
				ID:          partPathID(partNum),
				Filename:    filename,
				ContentType: fmt.Sprintf("%s/%s", bs.MIMEType, bs.MIMESubType),
				Size:        len(content),
				Content:     content,
			}
			attachment.Filename = fallbackString(attachment.Filename, "attachment-"+attachment.ID)

			attachments = append(attachments, attachment)
		}

		for i, part := range bs.Parts {
			newPartNum := childPartPath(partNum, i)
			if err := processAttachmentPart(part, newPartNum); err != nil {
				return err
			}
		}

		return nil
	}

	err := processAttachmentPart(msg.BodyStructure, nil)
	return attachments, err
}

func (c *Client) processMessage(msg *imap.Message) (models.Email, error) {
	email := models.Email{
		ID:    fmt.Sprintf("%d", msg.Uid),
		Flags: msg.Flags,
	}
	parsedAttachments := []models.Attachment{}
	attachmentCounter := 1

	// Process envelope information
	if msg.Envelope != nil {
		email.Subject = msg.Envelope.Subject
		email.Date = msg.Envelope.Date

		// Process From addresses
		if len(msg.Envelope.From) > 0 && msg.Envelope.From[0] != nil {
			email.From = msg.Envelope.From[0].Address()
			email.FromName = msg.Envelope.From[0].PersonalName
		}

		// Process To addresses
		if len(msg.Envelope.To) > 0 {
			var toAddresses []string
			var toNames []string
			for _, addr := range msg.Envelope.To {
				if addr != nil {
					toAddresses = append(toAddresses, addr.Address())
					if addr.PersonalName != "" {
						toNames = append(toNames, addr.PersonalName)
					}
				}
			}
			email.To = strings.Join(toAddresses, ", ")
			email.ToNames = toNames
		}

		// Process CC addresses
		if len(msg.Envelope.Cc) > 0 {
			var ccAddresses []string
			for _, addr := range msg.Envelope.Cc {
				if addr != nil {
					ccAddresses = append(ccAddresses, addr.Address())
				}
			}
			email.Cc = strings.Join(ccAddresses, ", ")
		}
	}

	// Process body
	// Process body
	var section imap.BodySectionName
	r := msg.GetBody(&section)
	if r != nil {
		// Read the body
		body, err := ioutil.ReadAll(r)
		if err != nil {
			return email, fmt.Errorf("error reading body: %v", err)
		}

		// Debug
		log.Printf("Initial body length: %d", len(body))

		// Parse the message
		m, err := mail.ReadMessage(bytes.NewReader(body))
		if err != nil {
			return email, fmt.Errorf("error parsing message: %v", err)
		}

		// Debug content type
		contentType := m.Header.Get("Content-Type")
		log.Printf("Content-Type: %s", contentType)

		bodyData, err := ioutil.ReadAll(m.Body)
		if err == nil {
			header := textproto.MIMEHeader{}
			header.Set("Content-Type", contentType)
			parseMessagePart(header, bodyData, &email, &parsedAttachments, &attachmentCounter)
		}

		// Add preview after all content is processed
		if email.Body != "" {
			email.Preview = createPreview(email.Body)
		} else if email.HTML != "" {
			stripped := stripHTML(string(email.HTML))
			email.Preview = createPreview(stripped)
		}

		if len(parsedAttachments) > 0 {
			email.Attachments = parsedAttachments
			email.HasAttachments = true
		}
	}

	// Debug final state
	log.Printf("Final state - Body: %d bytes, HTML: %d bytes, Preview: %d bytes",
		len(email.Body), len(string(email.HTML)), len(email.Preview))
	// Process attachments if needed
	attachments, err := c.processAttachments(msg)
	if err != nil {
		log.Printf("Warning: error processing attachments: %v", err)
	}
	if len(attachments) > 0 {
		email.Attachments = attachments
		email.HasAttachments = true
	}

	return email, nil
}

// Simple HTML tag stripping
func stripHTML(html string) string {
	var builder strings.Builder
	inTag := false

	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			builder.WriteRune(r)
		}
	}

	return strings.TrimSpace(builder.String())
}

func cleanPlainTextBody(body string) string {
	body = strings.TrimSpace(body)
	lines := strings.Split(body, "\n")
	var cleanedLines []string

	headerPattern := regexp.MustCompile(`^(From|To|Subject|Date|Message-ID|MIME-Version|Content-Type|Content-Transfer-Encoding):`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !headerPattern.MatchString(line) {
			cleanedLines = append(cleanedLines, line)
		}
	}

	return strings.Join(cleanedLines, "\n")
}

func createPreview(text string) string {
	// Normalize whitespace
	text = strings.Join(strings.Fields(text), " ")

	// Trim to preview length
	if len(text) > 150 {
		// Try to break at a word boundary
		if idx := strings.LastIndex(text[:150], " "); idx > 0 {
			return text[:idx] + "..."
		}
		return text[:150] + "..."
	}
	return text
}

func html2text(htmlStr string) string {
	// Simple HTML to text conversion
	text := strings.NewReplacer(
		"<br>", "\n",
		"<br/>", "\n",
		"<br />", "\n",
		"<p>", "\n",
		"</p>", "\n",
		"&nbsp;", " ",
	).Replace(htmlStr)

	// Remove remaining HTML tags
	text = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, "")

	// Decode HTML entities
	text = html.UnescapeString(text)

	// Clean up whitespace
	text = strings.TrimSpace(text)
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	return text
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
			section := buildTextSection(partNum)

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
			newPartNum := childPartPath(partNum, i)
			if body, found := findSection(part, newPartNum); found {
				return body, true
			}
		}

		return "", false
	}

	body, _ := findSection(msg.BodyStructure, nil)
	return body
}
