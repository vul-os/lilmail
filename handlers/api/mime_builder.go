// handlers/api/mime_builder.go — RFC 2822 + MIME message assembly.
//
// Builds multipart/mixed (body + attachments) and multipart/alternative
// (text/plain + text/html) messages correctly so that both the SMTP DATA
// stream and the IMAP APPEND payload are identical. This is the single source
// of truth for message construction; SendMail and SaveDraft both call it.
package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"time"
)

// Attachment holds the content of a file to be attached to an outgoing message.
//
// An attachment is one of two kinds:
//   - Regular attachment (Inline=false): rendered with
//     Content-Disposition: attachment in the outer multipart/mixed. This is a
//     downloadable file.
//   - Inline attachment (Inline=true, ContentID set): rendered with
//     Content-Disposition: inline and a Content-ID header inside a
//     multipart/related container, so an HTML body can reference it with
//     <img src="cid:CONTENT-ID">. This replaces the old "fat" data: URI that
//     forced the raw bytes to travel base64-inflated inside the HTML itself.
type OutgoingAttachment struct {
	Filename    string
	ContentType string // e.g. "application/pdf"; if empty, "application/octet-stream"
	Data        []byte

	// ContentID is the bare cid token (WITHOUT the surrounding angle brackets and
	// WITHOUT the "cid:" scheme). For an HTML body containing
	// <img src="cid:logo123">, set ContentID = "logo123". When set together with
	// Inline, the part is emitted with Content-ID: <logo123>. Ignored when Inline
	// is false. It is validated (see validateContentID) to prevent header injection.
	ContentID string

	// Inline marks this attachment as a body-referenced inline part rather than a
	// downloadable attachment. An inline attachment MUST carry a ContentID and is
	// only meaningful when the message has an HTML body.
	Inline bool
}

// isInline reports whether this attachment should be treated as an inline,
// Content-ID-referenced part. An inline flag with no ContentID is not usable as
// an inline part (nothing could reference it), so it degrades to a regular
// attachment.
func (a OutgoingAttachment) isInline() bool {
	return a.Inline && a.ContentID != ""
}

// MIMEMessageOptions carries everything needed to build a well-formed RFC 2822
// message. Either PlainBody or HTMLBody (or both) must be non-empty.
type MIMEMessageOptions struct {
	From        string
	To          string
	Cc          string
	Subject     string
	InReplyTo   string
	References  string
	MessageID   string // if empty, one is generated
	PlainBody   string
	HTMLBody    string // optional; if set, produces multipart/alternative
	Attachments []OutgoingAttachment
}

// BuildMIMEMessage assembles a complete RFC 2822 message and returns the raw
// bytes ready for SMTP DATA or IMAP APPEND. The function handles:
//   - Plain-text only → Content-Type: text/plain
//   - HTML + plain    → multipart/alternative (plain first, html second)
//   - Any of the above + regular attachments → multipart/mixed outer wrapper
//   - HTML + inline (cid:) attachments → the body + inline parts are wrapped in a
//     multipart/related container (HTML body as the root), and any regular
//     attachments wrap that related container in a multipart/mixed, i.e.:
//     mixed( related( alternative(text, html), inline-images... ), attachments... )
//     When there are no regular attachments the outer mixed is elided and the
//     related container is the top-level body.
func BuildMIMEMessage(opts MIMEMessageOptions) ([]byte, error) {
	if opts.PlainBody == "" && opts.HTMLBody == "" {
		return nil, fmt.Errorf("message must have at least a plain or HTML body")
	}

	// SMTP header-injection guard. From/To/Cc and the threading headers are written
	// verbatim into the RFC 2822 header block below; a CR/LF (or NUL) smuggled into
	// any of them would terminate the header and let a caller inject arbitrary
	// headers (e.g. a silent Bcc:) or split the message. These values originate
	// from the JSON send/draft body (handlers/jsonapi), so they are untrusted and
	// must fail closed here — the single choke point every send path funnels
	// through. Subject is Q-encoded (which neutralises CR/LF) so it is exempt.
	for _, f := range []struct {
		name, val string
	}{
		{"From", opts.From},
		{"To", opts.To},
		{"Cc", opts.Cc},
		{"In-Reply-To", opts.InReplyTo},
		{"References", opts.References},
		{"Message-ID", opts.MessageID},
	} {
		if err := validateHeaderValue(f.val); err != nil {
			return nil, fmt.Errorf("%s header: %w", f.name, err)
		}
	}

	// Partition attachments into inline (cid-referenced) and regular. Inline parts
	// are only meaningful with an HTML body; if there's no HTML body, an "inline"
	// attachment has nothing to reference, so treat it as a regular attachment.
	var inline, regular []OutgoingAttachment
	for _, att := range opts.Attachments {
		if att.isInline() && opts.HTMLBody != "" {
			if err := validateContentID(att.ContentID); err != nil {
				return nil, fmt.Errorf("attachment %q: %w", att.Filename, err)
			}
			inline = append(inline, att)
		} else {
			regular = append(regular, att)
		}
	}

	// Ensure a Message-ID.
	msgID := opts.MessageID
	if msgID == "" {
		domain := GetDomainFromEmail(opts.From)
		msgID = fmt.Sprintf("<%s@%s>", generateMsgID(), domain)
	}

	now := time.Now().Format(time.RFC822Z)
	username := GetUsernameFromEmail(opts.From)

	// Build headers.
	var hdr strings.Builder
	hdr.WriteString("Date: " + now + "\r\n")
	hdr.WriteString("From: " + username + " <" + opts.From + ">\r\n")
	hdr.WriteString("To: " + opts.To + "\r\n")
	if opts.Cc != "" {
		hdr.WriteString("Cc: " + opts.Cc + "\r\n")
	}
	hdr.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", opts.Subject) + "\r\n")
	hdr.WriteString("MIME-Version: 1.0\r\n")
	hdr.WriteString("Message-ID: " + msgID + "\r\n")
	if opts.InReplyTo != "" {
		hdr.WriteString("In-Reply-To: " + opts.InReplyTo + "\r\n")
	}
	if opts.References != "" {
		hdr.WriteString("References: " + opts.References + "\r\n")
	}

	// Build the body part (may be multipart/alternative if HTML is present).
	bodyBytes, bodyContentType, err := buildBodyPart(opts.PlainBody, opts.HTMLBody)
	if err != nil {
		return nil, err
	}

	// If there are inline (cid) parts, wrap the body + inline parts in a
	// multipart/related container. The related container then becomes the "body"
	// that the outer mixed (if any regular attachments) or the top level carries.
	if len(inline) > 0 {
		relBytes, relContentType, err := buildRelatedPart(bodyBytes, bodyContentType, inline)
		if err != nil {
			return nil, err
		}
		bodyBytes, bodyContentType = relBytes, relContentType
	}

	if len(regular) == 0 {
		// Simple: just add the Content-Type for the body (or related container)
		// and write it.
		hdr.WriteString("Content-Type: " + bodyContentType + "\r\n")
		hdr.WriteString("\r\n")
		var out bytes.Buffer
		out.WriteString(hdr.String())
		out.Write(bodyBytes)
		return out.Bytes(), nil
	}

	// With regular attachments: wrap everything in multipart/mixed.
	boundary := generateBoundary()
	hdr.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n")
	hdr.WriteString("\r\n")

	var out bytes.Buffer
	out.WriteString(hdr.String())

	mw := multipart.NewWriter(&out)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set boundary: %w", err)
	}

	// First part: the body (plain or alternative).
	bodyHdr := textproto.MIMEHeader{}
	bodyHdr.Set("Content-Type", bodyContentType)
	bw, err := mw.CreatePart(bodyHdr)
	if err != nil {
		return nil, fmt.Errorf("create body part: %w", err)
	}
	if _, err := bw.Write(bodyBytes); err != nil {
		return nil, fmt.Errorf("write body part: %w", err)
	}

	// Attachment parts.
	for _, att := range regular {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		attHdr := textproto.MIMEHeader{}
		attHdr.Set("Content-Type", ct)
		attHdr.Set("Content-Transfer-Encoding", "base64")
		attHdr.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.Filename))
		aw, err := mw.CreatePart(attHdr)
		if err != nil {
			return nil, fmt.Errorf("create attachment part %q: %w", att.Filename, err)
		}
		// Write base64-encoded content with 76-char line breaks (RFC 2045).
		if err := writeBase64Lines(aw, att.Data); err != nil {
			return nil, fmt.Errorf("write attachment data: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	return out.Bytes(), nil
}

// buildRelatedPart wraps the message body (the plain/alternative bytes produced
// by buildBodyPart) together with the inline, Content-ID-referenced attachments
// in a multipart/related container. Per RFC 2387 the root part is the HTML body
// (the first part), and each inline image follows as its own part carrying a
// Content-ID and Content-Disposition: inline. It returns the container bytes and
// its Content-Type (including the type="..." / boundary parameters).
//
// Callers must have already validated every inline ContentID.
func buildRelatedPart(bodyBytes []byte, bodyContentType string, inline []OutgoingAttachment) ([]byte, string, error) {
	boundary := generateBoundary()
	// type="text/html" advertises the root part's media type per RFC 2387. The
	// root is the multipart/alternative (or text/html) body part below, whose
	// renderable representation is HTML.
	ct := "multipart/related; type=\"text/html\"; boundary=\"" + boundary + "\""

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, "", err
	}

	// Root part: the message body (multipart/alternative or a single text part).
	rootHdr := textproto.MIMEHeader{}
	rootHdr.Set("Content-Type", bodyContentType)
	rw, err := mw.CreatePart(rootHdr)
	if err != nil {
		return nil, "", fmt.Errorf("create related root part: %w", err)
	}
	if _, err := rw.Write(bodyBytes); err != nil {
		return nil, "", fmt.Errorf("write related root part: %w", err)
	}

	// Inline image parts.
	for _, att := range inline {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		partHdr := textproto.MIMEHeader{}
		partHdr.Set("Content-Type", ct)
		partHdr.Set("Content-Transfer-Encoding", "base64")
		partHdr.Set("Content-ID", "<"+att.ContentID+">")
		// A filename is still useful (some clients surface inline images too).
		if att.Filename != "" {
			partHdr.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", att.Filename))
		} else {
			partHdr.Set("Content-Disposition", "inline")
		}
		pw, err := mw.CreatePart(partHdr)
		if err != nil {
			return nil, "", fmt.Errorf("create inline part %q: %w", att.ContentID, err)
		}
		if err := writeBase64Lines(pw, att.Data); err != nil {
			return nil, "", fmt.Errorf("write inline part %q: %w", att.ContentID, err)
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close related writer: %w", err)
	}
	return buf.Bytes(), ct, nil
}

// writeBase64Lines writes data as base64 with RFC 2045 76-char line breaks.
func writeBase64Lines(w io.Writer, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 76 {
		if _, err := w.Write([]byte(encoded[:76] + "\r\n")); err != nil {
			return err
		}
		encoded = encoded[76:]
	}
	if len(encoded) > 0 {
		if _, err := w.Write([]byte(encoded + "\r\n")); err != nil {
			return err
		}
	}
	return nil
}

// validateContentID rejects a Content-ID that could break the MIME structure or
// inject headers. A Content-ID travels inside "<...>" in a header line and is
// referenced from HTML as cid:<id>, so we constrain it to a conservative subset
// of RFC 2822 addr-spec / RFC 2392 characters: alphanumerics and a few safe
// punctuation marks, no whitespace, no CR/LF, no angle brackets or quotes. This
// keeps the header un-injectable and the cid: reference unambiguous.
func validateContentID(cid string) error {
	if cid == "" {
		return fmt.Errorf("empty Content-ID")
	}
	if len(cid) > 255 {
		return fmt.Errorf("Content-ID too long")
	}
	if !contentIDRe.MatchString(cid) {
		return fmt.Errorf("Content-ID %q contains illegal characters", cid)
	}
	return nil
}

// validateHeaderValue rejects a structured header value that would break the
// RFC 2822 header block. Any bare CR, LF, or NUL terminates the current header
// line (a folded continuation must begin with SP/HTAB, which callers here never
// produce), so their presence signals a header-injection attempt and is refused
// outright. Empty values are allowed — callers omit empty optional headers.
func validateHeaderValue(v string) error {
	if strings.ContainsAny(v, "\r\n\x00") {
		return fmt.Errorf("value contains illegal CR/LF/NUL (header injection)")
	}
	return nil
}

// contentIDRe is the allowed shape of a bare Content-ID token. It permits the
// common "local@domain" and "local" forms clients generate, but no separators
// that could terminate the header or the angle-bracket wrapper.
var contentIDRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+(@[A-Za-z0-9.\-]+)?$`)

// buildBodyPart builds the content bytes and Content-Type string for the body.
// If HTMLBody is empty, produces a plain text part.
// If both are present, produces a multipart/alternative part.
func buildBodyPart(plain, html string) ([]byte, string, error) {
	if html == "" {
		// Plain text only.
		var buf bytes.Buffer
		qpw := quotedprintable.NewWriter(&buf)
		if _, err := qpw.Write([]byte(plain)); err != nil {
			return nil, "", err
		}
		if err := qpw.Close(); err != nil {
			return nil, "", err
		}
		ct := "text/plain; charset=\"utf-8\"; format=flowed"
		// Prepend the transfer-encoding header inside the part bytes.
		var part bytes.Buffer
		part.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		part.Write(buf.Bytes())
		return part.Bytes(), ct, nil
	}

	// Both plain + HTML: multipart/alternative.
	boundary := generateBoundary()
	ct := "multipart/alternative; boundary=\"" + boundary + "\""

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, "", err
	}

	// Part 1: text/plain.
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Type", "text/plain; charset=\"utf-8\"")
	ph.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := mw.CreatePart(ph)
	if err != nil {
		return nil, "", err
	}
	qpw := quotedprintable.NewWriter(pw)
	if _, err := qpw.Write([]byte(plain)); err != nil {
		return nil, "", err
	}
	if err := qpw.Close(); err != nil {
		return nil, "", err
	}

	// Part 2: text/html.
	hh := textproto.MIMEHeader{}
	hh.Set("Content-Type", "text/html; charset=\"utf-8\"")
	hh.Set("Content-Transfer-Encoding", "quoted-printable")
	hw, err := mw.CreatePart(hh)
	if err != nil {
		return nil, "", err
	}
	qpwh := quotedprintable.NewWriter(hw)
	if _, err := qpwh.Write([]byte(html)); err != nil {
		return nil, "", err
	}
	if err := qpwh.Close(); err != nil {
		return nil, "", err
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}

	return buf.Bytes(), ct, nil
}

// generateBoundary creates a random MIME boundary string.
func generateBoundary() string {
	return fmt.Sprintf("__%d_%d_%d__",
		time.Now().UnixNano(),
		os.Getpid(),
		rand.Int63())
}

// generateMsgID creates a unique message-ID local part.
func generateMsgID() string {
	return fmt.Sprintf("%d.%d.%d",
		time.Now().UnixNano(),
		os.Getpid(),
		rand.Int63())
}
