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
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/emersion/go-imap"
)

// reMessageID matches RFC 2822 message-id tokens of the form <...>.
var reMessageID = regexp.MustCompile(`<[^>]+>`)

// previewSection is the IMAP body section used to fetch a small text snippet
// for the message list. BODY.PEEK[TEXT]<0.512> fetches the first 512 bytes of
// the message text body without implicitly setting the \Seen flag.
var previewSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{Specifier: imap.TextSpecifier},
	Peek:         true,
	Partial:      []int{0, 512},
}

// referencesSection fetches the References header line without marking seen.
var referencesSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{
		Specifier: imap.HeaderSpecifier,
		Fields:    []string{"References"},
	},
	Peek: true,
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
		imap.FetchBodyStructure,
		imap.FetchUid,
		previewSection.FetchItem(),    // partial TEXT body for preview snippet
		referencesSection.FetchItem(), // References header for JWZ threading
	}

	done := make(chan error, 1)
	go func() {
		done <- c.client.Fetch(seqSet, items, messages)
	}()

	var emails []models.Email
	for msg := range messages {
		email, err := c.processListMessage(msg, folderName)
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

// processListMessage builds an Email from a list-fetch message. It only
// populates envelope/flags/attachments and derives a short preview from the
// partial TEXT body section. It does NOT parse the full body or HTML.
func (c *Client) processListMessage(msg *imap.Message, folderName string) (models.Email, error) {
	email := models.Email{
		ID:    fmt.Sprintf("%d", msg.Uid),
		Flags: msg.Flags,
	}

	if msg.Envelope != nil {
		email.Subject = msg.Envelope.Subject
		email.Date = msg.Envelope.Date

		if len(msg.Envelope.From) > 0 && msg.Envelope.From[0] != nil {
			email.From = msg.Envelope.From[0].Address()
			email.FromName = msg.Envelope.From[0].PersonalName
		}

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

		// Threading headers from the envelope.
		email.MessageID = msg.Envelope.MessageId
		email.InReplyTo = msg.Envelope.InReplyTo
	}

	// References is not in the envelope — extract from the header section.
	if r := msg.GetBody(referencesSection); r != nil {
		if raw, err := ioutil.ReadAll(r); err == nil {
			email.References = reMessageID.FindAllString(string(raw), -1)
		}
	}

	// Build preview from the partial TEXT section if available.
	if r := msg.GetBody(previewSection); r != nil {
		if raw, err := ioutil.ReadAll(r); err == nil && len(raw) > 0 {
			text := string(raw)
			// Strip any residual MIME headers that appear before the actual body.
			if idx := strings.Index(text, "\r\n\r\n"); idx >= 0 {
				text = text[idx+4:]
			} else if idx := strings.Index(text, "\n\n"); idx >= 0 {
				text = text[idx+2:]
			}
			text = stripHTML(text)
			email.Preview = createPreview(text)
		}
	}

	// Attachment metadata (content fetched on-demand).
	attachments := c.processAttachments(msg, folderName)
	email.Attachments = attachments
	email.HasAttachments = len(attachments) > 0

	return email, nil
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

	return c.processMessage(msg, folderName)
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

// processAttachments builds attachment metadata from the message's
// BODYSTRUCTURE. Content is intentionally NOT downloaded here — it would mean
// pulling every attachment of every listed message into memory. Instead each
// attachment carries an ID that the download route (FetchAttachment) uses to
// fetch the specific MIME part on demand.
func (c *Client) processAttachments(msg *imap.Message, folderName string) []models.Attachment {
	if msg.BodyStructure == nil {
		return nil
	}

	var attachments []models.Attachment
	uid := fmt.Sprintf("%d", msg.Uid)

	msg.BodyStructure.Walk(func(path []int, part *imap.BodyStructure) bool {
		if part != nil && isAttachmentPart(part) {
			filename, _ := part.Filename()
			if filename == "" {
				filename = "attachment"
			}
			attachments = append(attachments, models.Attachment{
				ID:          encodeAttachmentID(folderName, uid, pathToString(path)),
				Filename:    filename,
				ContentType: fmt.Sprintf("%s/%s", strings.ToLower(part.MIMEType), strings.ToLower(part.MIMESubType)),
				Size:        int(part.Size),
			})
		}
		return true // keep walking children
	})

	return attachments
}

// isAttachmentPart reports whether a MIME part should be treated as a
// downloadable attachment.
func isAttachmentPart(bs *imap.BodyStructure) bool {
	if strings.EqualFold(bs.Disposition, "attachment") {
		return true
	}
	// Inline (or undeclared) non-text parts with a filename — e.g. images.
	if strings.EqualFold(bs.MIMEType, "multipart") || strings.EqualFold(bs.MIMEType, "text") {
		return false
	}
	if fn, _ := bs.Filename(); fn != "" {
		return true
	}
	return false
}

// pathToString renders an IMAP part path (e.g. []int{2,1}) as "2.1".
func pathToString(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// encodeAttachmentID packs the folder, UID, and part path into a single
// URL-safe token used in the attachment download URL.
func encodeAttachmentID(folder, uid, part string) string {
	raw := folder + "\x00" + uid + "\x00" + part
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeAttachmentID reverses encodeAttachmentID.
func DecodeAttachmentID(id string) (folder, uid, part string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid attachment id: %v", err)
	}
	fields := strings.SplitN(string(raw), "\x00", 3)
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("malformed attachment id")
	}
	return fields[0], fields[1], fields[2], nil
}

// FetchAttachment downloads and decodes a single MIME part on demand. It
// returns the decoded content, the filename, and the content type.
func (c *Client) FetchAttachment(folderName, uid, partPath string) ([]byte, string, string, error) {
	uidNum, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid UID: %v", err)
	}

	path, err := parsePartPath(partPath)
	if err != nil {
		return nil, "", "", err
	}

	if _, err := c.client.Select(folderName, true); err != nil {
		return nil, "", "", fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uint32(uidNum))

	// Request the specific part body (BODY[<path>]) plus the structure so we
	// can read the part's filename, content type, and transfer encoding.
	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{Path: path},
		Peek:         true,
	}
	items := []imap.FetchItem{imap.FetchBodyStructure, section.FetchItem()}

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
		return nil, "", "", fmt.Errorf("fetch error: %v", err)
	}
	if msg == nil {
		return nil, "", "", fmt.Errorf("attachment not found")
	}

	r := msg.GetBody(section)
	if r == nil {
		return nil, "", "", fmt.Errorf("no body for attachment part %s", partPath)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, "", "", fmt.Errorf("error reading attachment: %v", err)
	}

	// Resolve metadata for this part from the structure.
	filename := "attachment"
	contentType := "application/octet-stream"
	encoding := ""
	if msg.BodyStructure != nil {
		msg.BodyStructure.Walk(func(p []int, part *imap.BodyStructure) bool {
			if part != nil && samePath(p, path) {
				if fn, _ := part.Filename(); fn != "" {
					filename = fn
				}
				contentType = fmt.Sprintf("%s/%s", strings.ToLower(part.MIMEType), strings.ToLower(part.MIMESubType))
				encoding = strings.ToLower(part.Encoding)
			}
			return true
		})
	}

	content, err := decodeContent(raw, encoding)
	if err != nil {
		return nil, "", "", fmt.Errorf("error decoding attachment: %v", err)
	}
	return content, filename, contentType, nil
}

// parsePartPath parses an IMAP part path such as "2.1" into []int{2, 1}.
func parsePartPath(s string) ([]int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty part path")
	}
	fields := strings.Split(s, ".")
	path := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("invalid part path %q", s)
		}
		path = append(path, n)
	}
	return path, nil
}

func samePath(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeContent decodes a MIME part body according to its transfer encoding.
func decodeContent(raw []byte, encoding string) ([]byte, error) {
	switch encoding {
	case "base64":
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(raw))
		return base64.StdEncoding.DecodeString(cleaned)
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return raw, nil // fall back to the raw bytes
		}
		return decoded, nil
	default:
		return raw, nil
	}
}

func (c *Client) processMessage(msg *imap.Message, folderName string) (models.Email, error) {
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

		// Threading headers from the envelope.
		email.MessageID = msg.Envelope.MessageId
		email.InReplyTo = msg.Envelope.InReplyTo
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

		// Extract References header for threading (envelope doesn't carry it).
		if refsHdr := m.Header.Get("References"); refsHdr != "" {
			email.References = reMessageID.FindAllString(refsHdr, -1)
		}

		// Debug content type
		contentType := m.Header.Get("Content-Type")
		log.Printf("Content-Type: %s", contentType)

		// Handle multipart messages
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err == nil && strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(m.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Printf("Error getting next part: %v", err)
					continue
				}

				// Debug part content type
				log.Printf("Part Content-Type: %s", p.Header.Get("Content-Type"))

				// Read the part
				partData, err := ioutil.ReadAll(p)
				if err != nil {
					log.Printf("Error reading part: %v", err)
					continue
				}

				// Debug part length
				log.Printf("Part length: %d", len(partData))

				partType := p.Header.Get("Content-Type")
				switch {
				case strings.Contains(partType, "text/plain"):
					email.Body = string(partData)
					log.Printf("Found plain text: %d bytes", len(email.Body))
				case strings.Contains(partType, "text/html"):
					email.HTML = template.HTML(partData)
					log.Printf("Found HTML: %d bytes", len(string(email.HTML)))
				}
			}
		} else {
			// Handle non-multipart messages
			bodyData, err := ioutil.ReadAll(m.Body)
			if err == nil {
				email.Body = string(bodyData)
				log.Printf("Non-multipart body: %d bytes", len(email.Body))
			}
		}

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
	// Attachment metadata (content is fetched on demand by FetchAttachment).
	attachments := c.processAttachments(msg, folderName)
	email.Attachments = attachments
	email.HasAttachments = len(attachments) > 0

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
