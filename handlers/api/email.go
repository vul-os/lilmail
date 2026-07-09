// handlers/api/email.go
package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	stdhtml "html"
	"io"
	"lilmail/models"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"github.com/emersion/go-imap"
)

// reMessageID matches RFC 2822 message-id tokens of the form <...>.
var reMessageID = regexp.MustCompile(`<[^>]+>`)

// previewSection is the IMAP body section used to fetch a small text snippet
// for the message list. BODY.PEEK[TEXT]<0.4096> fetches the head of the message
// text body without implicitly setting the \Seen flag. 4096 rather than 512
// because an HTML mail opens with a <style> block that routinely runs past the
// first kilobyte, leaving no prose in a shorter window.
var previewSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{Specifier: imap.TextSpecifier},
	Peek:         true,
	Partial:      []int{0, 4096},
}

// referencesSection fetches the References header line without marking seen.
var referencesSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{
		Specifier: imap.HeaderSpecifier,
		Fields:    []string{"References"},
	},
	Peek: true,
}

// FetchMessages retrieves the newest `limit` messages from a specified folder.
func (c *Client) FetchMessages(folderName string, limit uint32) ([]models.Email, error) {
	return c.FetchMessagesPaged(folderName, limit, 0)
}

// FetchMessagesPaged retrieves up to `limit` messages from folderName, skipping
// the newest `offset` messages first. IMAP sequence numbers ascend oldest→newest,
// so the window [end-limit+1, end] where end = total-offset yields a newest-first
// page. offset=0 returns the newest `limit` (identical to FetchMessages); larger
// offsets scroll further back, letting a client page a large mailbox.
func (c *Client) FetchMessagesPaged(folderName string, limit, offset uint32) ([]models.Email, error) {
	mbox, err := c.client.Select(folderName, false)
	if err != nil {
		return nil, fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	if mbox.Messages == 0 || offset >= mbox.Messages {
		return []models.Email{}, nil
	}

	end := mbox.Messages - offset
	from := uint32(1)
	if end > limit {
		from = end - limit + 1
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, end)

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
			log.Printf("email: processListMessage uid=%d: %v", msg.Uid, err)
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
		if raw, err := io.ReadAll(r); err == nil {
			email.References = reMessageID.FindAllString(string(raw), -1)
		}
	}

	// Build preview from the partial TEXT section if available.
	if r := msg.GetBody(previewSection); r != nil {
		if raw, err := io.ReadAll(r); err == nil && len(raw) > 0 {
			text := string(raw)
			// Strip any residual MIME headers that appear before the actual body.
			if idx := strings.Index(text, "\r\n\r\n"); idx >= 0 {
				text = text[idx+4:]
			} else if idx := strings.Index(text, "\n\n"); idx >= 0 {
				text = text[idx+2:]
			}
			// These bytes are still transfer-encoded: base64 shows up as gibberish
			// and quoted-printable leaks "=E2=80=94" and "=" soft line breaks.
			text = string(decodeTransferBytes([]byte(text), previewEncoding(msg.BodyStructure)))
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

// MoveMessage moves a message identified by its UID from srcFolder to
// destFolder. It first tries the IMAP MOVE extension (UidMove); if the server
// does not support it, it falls back to a copy + mark \Deleted + Expunge, which
// achieves the same effect on servers without RFC 6851.
func (c *Client) MoveMessage(srcFolder, uid, destFolder string) error {
	uidNum, err := parseUID(uid)
	if err != nil {
		return fmt.Errorf("invalid UID: %v", err)
	}

	if _, err := c.client.Select(srcFolder, false); err != nil {
		return fmt.Errorf("error selecting folder %s: %v", srcFolder, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	// Preferred path: server-side MOVE (RFC 6851).
	if err := c.client.UidMove(seqSet, destFolder); err == nil {
		return nil
	}

	// Fallback: copy to destination, then mark the source as deleted and expunge.
	if err := c.client.UidCopy(seqSet, destFolder); err != nil {
		return fmt.Errorf("error copying message to %s: %v", destFolder, err)
	}

	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}
	if err := c.client.UidStore(seqSet, item, flags, nil); err != nil {
		return fmt.Errorf("error marking message as deleted: %v", err)
	}
	if err := c.client.Expunge(nil); err != nil {
		return fmt.Errorf("error expunging mailbox: %v", err)
	}
	return nil
}

// SetMessageFlag sets or removes the given IMAP flag on a message identified
// by its UID in folderName.  add=true adds the flag; add=false removes it.
func (c *Client) SetMessageFlag(folderName, uid string, flag string, add bool) error {
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
			partID := pathToString(path)
			attachments = append(attachments, models.Attachment{
				ID:          encodeAttachmentID(folderName, uid, partID),
				PartID:      partID,
				Filename:    filename,
				ContentType: fmt.Sprintf("%s/%s", strings.ToLower(part.MIMEType), strings.ToLower(part.MIMESubType)),
				Size:        int(part.Size),
				IsInline:    strings.EqualFold(part.Disposition, "inline"),
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
	var section imap.BodySectionName
	r := msg.GetBody(&section)
	if r != nil {
		// Read the body
		body, err := io.ReadAll(r)
		if err != nil {
			return email, fmt.Errorf("error reading body: %v", err)
		}

		// Parse the message
		m, err := mail.ReadMessage(bytes.NewReader(body))
		if err != nil {
			return email, fmt.Errorf("error parsing message: %v", err)
		}

		// Extract References header for threading (envelope doesn't carry it).
		if refsHdr := m.Header.Get("References"); refsHdr != "" {
			email.References = reMessageID.FindAllString(refsHdr, -1)
		}

		// Surface the receiving server's SPF/DKIM/DMARC verdict (RFC 8601). A
		// message may carry several Authentication-Results headers (one per hop);
		// pass them all. Nil when absent/unparseable — no badge then.
		if ar := m.Header["Authentication-Results"]; len(ar) > 0 {
			email.Auth = ParseAuthResults(ar)
		}

		// Surface any List-Unsubscribe (RFC 2369) + List-Unsubscribe-Post (RFC
		// 8058) targets so the reading pane can offer a one-click Unsubscribe
		// button. Read-only — lilmail never dereferences the URL; the client
		// validates the scheme, confirms, and acts. Nil when absent/unsupported.
		if lu := m.Header["List-Unsubscribe"]; len(lu) > 0 {
			email.Unsubscribe = ParseUnsubscribe(lu, m.Header["List-Unsubscribe-Post"])
		}

		collectBodies(
			m.Body,
			m.Header.Get("Content-Type"),
			m.Header.Get("Content-Transfer-Encoding"),
			m.Header.Get("Content-Disposition"),
			&email,
			0,
		)

		// Add preview after all content is processed
		if email.Body != "" {
			email.Preview = createPreview(email.Body)
		} else if email.HTML != "" {
			stripped := stripHTML(string(email.HTML))
			email.Preview = createPreview(stripped)
		}
	}

	// Attachment metadata (content is fetched on demand by FetchAttachment).
	attachments := c.processAttachments(msg, folderName)
	email.Attachments = attachments
	email.HasAttachments = len(attachments) > 0

	// iMIP: detect a text/calendar part and, if it carries an iTIP METHOD, attach
	// the parsed invite so the reading pane can render an RSVP card. The raw
	// message bytes were read above; re-parse them for the calendar walk (cheap,
	// single message). Viewer identity for MyPartStat is refined by the jsonapi
	// layer via fromEmail; here we use the login username as a best effort.
	if r := msg.GetBody(&imap.BodySectionName{}); r != nil {
		if raw, err := io.ReadAll(r); err == nil {
			if cal := extractCalendarPart(raw); cal != nil {
				if inv, err := ParseInvite(cal, c.username); err == nil && inv != nil {
					email.Invite = inv
				}
			}
		}
	}

	return email, nil
}

// decodeTransferBytes reverses a Content-Transfer-Encoding over a byte slice.
// Unknown or absent encodings (7bit/8bit/binary) pass through untouched.
// mime/multipart already decodes quoted-printable parts and strips the header,
// so that case here only fires for non-multipart messages, which net/mail
// leaves raw. Unlike the streaming decodeTransfer below it tolerates truncated
// input — the message-list preview decodes only
// the first 512 bytes of a body — by keeping whatever decoded cleanly before
// the cut rather than discarding the whole chunk.
func decodeTransferBytes(data []byte, enc string) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		// Transport base64 is line-wrapped; the decoder rejects newlines.
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, data)
		// The preview window can run past this part into the next MIME boundary,
		// so stop at the first byte outside the alphabet rather than failing the
		// whole chunk, then drop the trailing partial quantum.
		clean = clean[:base64PrefixLen(clean)]
		clean = clean[:len(clean)-len(clean)%4]
		if dec, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
			return dec
		}
	case "quoted-printable":
		var out bytes.Buffer
		// CopyN-style drain: a decode error mid-stream still leaves the prefix.
		if _, err := io.Copy(&out, quotedprintable.NewReader(bytes.NewReader(data))); err == nil || out.Len() > 0 {
			return out.Bytes()
		}
	}
	return data
}

// base64PrefixLen reports the length of the leading run of standard base64
// characters in b, i.e. where a decodable prefix stops.
func base64PrefixLen(b []byte) int {
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '=':
		default:
			return i
		}
	}
	return len(b)
}

// previewEncoding reports the Content-Transfer-Encoding that applies to the
// bytes IMAP returns for BODY[TEXT]: the message encoding for a single-part
// message, or the first leaf part's for a multipart one (BODY[TEXT] hands back
// the raw MIME body, whose first part is what the preview text is sliced from).
func previewEncoding(bs *imap.BodyStructure) string {
	if bs == nil {
		return ""
	}
	if strings.EqualFold(bs.MIMEType, "multipart") {
		if len(bs.Parts) == 0 {
			return ""
		}
		return previewEncoding(bs.Parts[0])
	}
	return bs.Encoding
}

// collectBodies fills email.Body (text/plain) and email.HTML (text/html) from a
// MIME tree, recursing through multipart containers and transfer-decoding each
// leaf. The first non-empty part of each kind wins, so a multipart/alternative
// keeps the plain part rather than letting a later sibling clobber it.
//
// A single-part text/html message must land in email.HTML, not email.Body — the
// viewer template renders HTML in a sandboxed iframe and only falls back to
// linkified plain text, so misfiling it shows the reader raw HTML source.
func collectBodies(body io.Reader, contentType, cte, disposition string, email *models.Email, depth int) {
	if depth > 10 {
		return
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A missing or unparseable Content-Type defaults to text/plain (RFC 2045).
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}
			collectBodies(
				p,
				p.Header.Get("Content-Type"),
				p.Header.Get("Content-Transfer-Encoding"),
				p.Header.Get("Content-Disposition"),
				email,
				depth+1,
			)
		}
	}

	// Attachments carry their own text; they must not become the body.
	if disp, _, err := mime.ParseMediaType(disposition); err == nil && disp == "attachment" {
		return
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return
	}
	data = decodeTransferBytes(data, cte)

	switch {
	case strings.HasPrefix(mediaType, "text/html") && email.HTML == "":
		email.HTML = string(data)
	case strings.HasPrefix(mediaType, "text/plain") && email.Body == "":
		email.Body = string(data)
	}
}

// extractCalendarPart walks a raw RFC 5322 message and returns the transfer-
// decoded bytes of the first text/calendar part, or nil when none is present.
// It recurses into nested multipart containers (an iMIP invite is commonly
// multipart/mixed › multipart/alternative › text/calendar) and decodes
// base64/quoted-printable so ParseInvite receives clean iCalendar text.
func extractCalendarPart(raw []byte) []byte {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	ct := m.Header.Get("Content-Type")
	cte := m.Header.Get("Content-Transfer-Encoding")
	return walkForCalendar(m.Body, ct, cte, 0)
}

// walkForCalendar recursively scans a MIME body for a text/calendar part.
func walkForCalendar(body io.Reader, contentType, transferEnc string, depth int) []byte {
	if depth > 10 {
		return nil // guard against pathological nesting
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}

	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			pct := p.Header.Get("Content-Type")
			pcte := p.Header.Get("Content-Transfer-Encoding")
			if found := walkForCalendar(p, pct, pcte, depth+1); found != nil {
				return found
			}
		}
		return nil
	}

	if mediaType == "text/calendar" {
		data, err := io.ReadAll(decodeTransfer(body, transferEnc))
		if err != nil {
			return nil
		}
		return data
	}
	return nil
}

// decodeTransfer wraps r with the appropriate decoder for a Content-Transfer-
// Encoding value (base64 / quoted-printable), or returns r unchanged.
func decodeTransfer(r io.Reader, enc string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r
	}
}

// Simple HTML tag stripping
// stripHTML reduces an HTML body to the text a preview snippet should show.
// Beyond dropping tags it skips <style>/<script> contents — otherwise a preview
// of a marketing mail is just its CSS reset ("#outlook a { padding:0; }") — then
// resolves entities and removes the zero-width characters senders pad their
// preheader with.
func stripHTML(markup string) string {
	var builder strings.Builder

	lower := strings.ToLower(markup)
	for i := 0; i < len(markup); {
		if markup[i] == '<' {
			// Skip comments, including Outlook conditionals such as
			// <!--[if mso]><o:PixelsPerInch>96</o:PixelsPerInch><![endif]-->,
			// whose contents are markup rather than prose.
			if strings.HasPrefix(markup[i:], "<!--") {
				if end := strings.Index(markup[i+4:], "-->"); end >= 0 {
					i += 4 + end + 3
					continue
				}
				break
			}
			// Skip an entire <style>…</style> or <script>…</script> element.
			if skipTo, ok := skipRawTextElement(lower, i); ok {
				i = skipTo
				continue
			}
			if end := strings.IndexByte(markup[i:], '>'); end >= 0 {
				i += end + 1
				continue
			}
			break // unterminated tag: nothing further is text
		}
		next := strings.IndexByte(markup[i:], '<')
		if next < 0 {
			builder.WriteString(markup[i:])
			break
		}
		builder.WriteString(markup[i : i+next])
		i += next
	}

	text := stdhtml.UnescapeString(builder.String())
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad', '\u034f':
			return -1 // zero-width padding used to pad inbox preheaders
		}
		return r
	}, text)

	return strings.TrimSpace(text)
}

// skipRawTextElement reports the offset just past the closing tag of a <style>
// or <script> element starting at i, whose contents are CDATA rather than text.
func skipRawTextElement(lower string, i int) (int, bool) {
	for _, name := range []string{"style", "script"} {
		open := "<" + name
		if !strings.HasPrefix(lower[i:], open) {
			continue
		}
		// Guard against matching a prefix such as <styles-are-not-a-tag>.
		if rest := lower[i+len(open):]; rest != "" && (isASCIILetter(rest[0]) || rest[0] == '-') {
			continue
		}
		closing := "</" + name
		if end := strings.Index(lower[i:], closing); end >= 0 {
			after := i + end + len(closing)
			if gt := strings.IndexByte(lower[after:], '>'); gt >= 0 {
				return after + gt + 1, true
			}
			return len(lower), true
		}
		return len(lower), true // unterminated: the rest is not text
	}
	return 0, false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
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
