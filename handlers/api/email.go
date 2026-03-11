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
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/emersion/go-imap"
)

func filenameFromBodyStructure(bs *imap.BodyStructure) string {
	if bs == nil {
		return ""
	}

	if filename, ok := bs.DispositionParams["filename"]; ok && strings.TrimSpace(filename) != "" {
		return strings.TrimSpace(filename)
	}

	if filename, ok := bs.DispositionParams["name"]; ok && strings.TrimSpace(filename) != "" {
		return strings.TrimSpace(filename)
	}

	if filename, ok := bs.Params["name"]; ok && strings.TrimSpace(filename) != "" {
		return strings.TrimSpace(filename)
	}

	if filename, ok := bs.Params["filename"]; ok && strings.TrimSpace(filename) != "" {
		return strings.TrimSpace(filename)
	}

	return ""
}

func isAttachmentPart(bs *imap.BodyStructure) bool {
	if bs == nil {
		return false
	}

	disposition := strings.ToLower(strings.TrimSpace(bs.Disposition))
	mimeType := strings.ToLower(strings.TrimSpace(bs.MIMEType))
	mimeSubType := strings.ToLower(strings.TrimSpace(bs.MIMESubType))
	filename := filenameFromBodyStructure(bs)

	if disposition == "attachment" {
		return true
	}

	if disposition == "inline" && mimeType != "text" {
		return true
	}

	if filename != "" {
		return !(mimeType == "text" && (mimeSubType == "plain" || mimeSubType == "html"))
	}

	return false
}

func bodySectionPathID(path []int) string {
	if len(path) == 0 {
		return ""
	}

	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strconv.Itoa(p)
	}

	return strings.Join(parts, ".")
}

func fetchBodyPartContent(msg *imap.Message, partPath []int) ([]byte, error) {
	section := &imap.BodySectionName{Peek: true}
	section.Path = append([]int(nil), partPath...)

	r := msg.GetBody(section)
	if r == nil {
		return nil, fmt.Errorf("no body for part %v", partPath)
	}

	content, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("error reading part %v: %v", partPath, err)
	}

	return content, nil
}

func decodeByTransferEncoding(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return data, nil
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data)))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	default:
		return data, nil
	}
}

func parseMIMEPart(header textproto.MIMEHeader, body io.Reader, email *models.Email, attachments *[]models.Attachment, nextAttachmentID *int) error {
	contentTypeHeader := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentTypeHeader)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
		params = map[string]string{}
	}

	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil
		}

		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			if err := parseMIMEPart(part.Header, part, email, attachments, nextAttachmentID); err != nil {
				part.Close()
				return err
			}
			part.Close()
		}

		return nil
	}

	rawData, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	decodedData, err := decodeByTransferEncoding(rawData, header.Get("Content-Transfer-Encoding"))
	if err != nil {
		decodedData = rawData
	}

	disposition, dispParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := strings.TrimSpace(dispParams["filename"])
	if filename == "" {
		filename = strings.TrimSpace(params["name"])
	}

	lowerMediaType := strings.ToLower(mediaType)
	isText := lowerMediaType == "text/plain" || lowerMediaType == "text/html"
	isAttachment := strings.EqualFold(disposition, "attachment") || (!isText && filename != "") || (strings.EqualFold(disposition, "inline") && !isText)

	if isAttachment {
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d", *nextAttachmentID)
		}

		*attachments = append(*attachments, models.Attachment{
			ID:          strconv.Itoa(*nextAttachmentID),
			Filename:    filename,
			ContentType: mediaType,
			Size:        len(decodedData),
			Content:     decodedData,
		})
		*nextAttachmentID++
		return nil
	}

	if lowerMediaType == "text/plain" && email.Body == "" {
		email.Body = string(decodedData)
	}

	if lowerMediaType == "text/html" && email.HTML == "" {
		email.HTML = template.HTML(decodedData)
	}

	return nil
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

		if isAttachmentPart(bs) {
			content, err := fetchBodyPartContent(msg, partNum)
			if err != nil {
				return err
			}

			filename := filenameFromBodyStructure(bs)
			if filename == "" {
				filename = "attachment-" + bodySectionPathID(partNum)
			}

			attachment := models.Attachment{
				ID:          bodySectionPathID(partNum),
				Filename:    filename,
				ContentType: fmt.Sprintf("%s/%s", bs.MIMEType, bs.MIMESubType),
				Size:        len(content),
				Content:     content,
			}

			attachments = append(attachments, attachment)
		}

		for i, part := range bs.Parts {
			newPartNum := append(append([]int{}, partNum...), i+1)
			if err := processAttachmentPart(part, newPartNum); err != nil {
				return err
			}
		}

		return nil
	}

	err := processAttachmentPart(msg.BodyStructure, nil)
	sort.SliceStable(attachments, func(i, j int) bool {
		return attachments[i].ID < attachments[j].ID
	})

	return attachments, err
}

func (c *Client) processMessage(msg *imap.Message) (models.Email, error) {
	email := models.Email{
		ID:    fmt.Sprintf("%d", msg.Uid),
		Flags: msg.Flags,
	}

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

	// Process body and attachments from raw RFC822 message
	var section imap.BodySectionName
	r := msg.GetBody(&section)
	if r != nil {
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

		attachments := []models.Attachment{}
		nextAttachmentID := 1
		if err := parseMIMEPart(textproto.MIMEHeader(m.Header), m.Body, &email, &attachments, &nextAttachmentID); err != nil {
			log.Printf("Warning: MIME parse issue: %v", err)
		}
		email.Attachments = attachments
		email.HasAttachments = len(attachments) > 0

		// Add preview after all content is processed
		if email.Body != "" {
			email.Preview = createPreview(email.Body)
		} else if email.HTML != "" {
			stripped := stripHTML(string(email.HTML))
			email.Preview = createPreview(stripped)
		}
	}

	// Debug final state
	log.Printf("Final state - Body: %d bytes, HTML: %d bytes, Preview: %d bytes",
		len(email.Body), len(string(email.HTML)), len(email.Preview))

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
			body, err := fetchBodyPartContent(msg, partNum)
			if err != nil {
				return "", false
			}

			return string(body), true
		}

		for i, part := range bs.Parts {
			newPartNum := append(append([]int{}, partNum...), i+1)
			if body, found := findSection(part, newPartNum); found {
				return body, true
			}
		}

		return "", false
	}

	body, _ := findSection(msg.BodyStructure, nil)
	return body
}
