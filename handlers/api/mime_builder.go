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
	"math/rand"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"strings"
	"time"
)

// Attachment holds the content of a file to be attached to an outgoing message.
type OutgoingAttachment struct {
	Filename    string
	ContentType string // e.g. "application/pdf"; if empty, "application/octet-stream"
	Data        []byte
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
//   - Any of the above + attachments → multipart/mixed outer wrapper
func BuildMIMEMessage(opts MIMEMessageOptions) ([]byte, error) {
	if opts.PlainBody == "" && opts.HTMLBody == "" {
		return nil, fmt.Errorf("message must have at least a plain or HTML body")
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

	if len(opts.Attachments) == 0 {
		// Simple: just add the Content-Type for the body and write it.
		hdr.WriteString("Content-Type: " + bodyContentType + "\r\n")
		hdr.WriteString("\r\n")
		var out bytes.Buffer
		out.WriteString(hdr.String())
		out.Write(bodyBytes)
		return out.Bytes(), nil
	}

	// With attachments: wrap everything in multipart/mixed.
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
	for _, att := range opts.Attachments {
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
		encoded := base64.StdEncoding.EncodeToString(att.Data)
		for len(encoded) > 76 {
			if _, err := aw.Write([]byte(encoded[:76] + "\r\n")); err != nil {
				return nil, fmt.Errorf("write attachment data: %w", err)
			}
			encoded = encoded[76:]
		}
		if len(encoded) > 0 {
			if _, err := aw.Write([]byte(encoded + "\r\n")); err != nil {
				return nil, fmt.Errorf("write attachment data tail: %w", err)
			}
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	return out.Bytes(), nil
}

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
