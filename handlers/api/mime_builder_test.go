// handlers/api/mime_builder_test.go — tests for MIME message assembly.
package api

import (
	"bytes"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// parseMail parses raw RFC 2822 bytes into a *mail.Message.
func parseMail(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse mail: %v", err)
	}
	return msg
}

func TestBuildMIMEMessage_PlainOnly(t *testing.T) {
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:      "alice@example.com",
		To:        "bob@example.com",
		Subject:   "Hello world",
		PlainBody: "Just plain text.",
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)

	if got := msg.Header.Get("To"); got != "bob@example.com" {
		t.Errorf("To: got %q, want %q", got, "bob@example.com")
	}
	ct := msg.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: got %q, want text/plain prefix", ct)
	}
	if !strings.Contains(msg.Header.Get("From"), "alice@example.com") {
		t.Errorf("From header missing address, got %q", msg.Header.Get("From"))
	}
	if msg.Header.Get("Message-ID") == "" {
		t.Error("Message-ID is missing")
	}
	if msg.Header.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version: got %q", msg.Header.Get("MIME-Version"))
	}
}

func TestBuildMIMEMessage_HTMLAndPlain(t *testing.T) {
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:      "alice@example.com",
		To:        "bob@example.com",
		Subject:   "Rich text",
		PlainBody: "Plain fallback.",
		HTMLBody:  "<p>Rich <b>HTML</b> body.</p>",
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	ct := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content-type %q: %v", ct, err)
	}
	if mediaType != "multipart/alternative" {
		t.Errorf("Content-Type: got %q, want multipart/alternative", mediaType)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	var partTypes []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		partTypes = append(partTypes, p.Header.Get("Content-Type"))
	}

	if len(partTypes) != 2 {
		t.Fatalf("expected 2 parts, got %d: %v", len(partTypes), partTypes)
	}
	if !strings.HasPrefix(partTypes[0], "text/plain") {
		t.Errorf("first part: got %q, want text/plain", partTypes[0])
	}
	if !strings.HasPrefix(partTypes[1], "text/html") {
		t.Errorf("second part: got %q, want text/html", partTypes[1])
	}
}

func TestBuildMIMEMessage_WithAttachments(t *testing.T) {
	att := OutgoingAttachment{
		Filename:    "test.pdf",
		ContentType: "application/pdf",
		Data:        []byte("%PDF-1.4 fake content"),
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:        "alice@example.com",
		To:          "bob@example.com",
		Subject:     "See attached",
		PlainBody:   "Please find the file attached.",
		Attachments: []OutgoingAttachment{att},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	ct := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	if mediaType != "multipart/mixed" {
		t.Errorf("Content-Type: got %q, want multipart/mixed", mediaType)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	partCount := 0
	var attDisp string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		partCount++
		if d := p.Header.Get("Content-Disposition"); strings.HasPrefix(d, "attachment") {
			attDisp = d
		}
	}
	if partCount != 2 {
		t.Errorf("expected 2 parts (body + attachment), got %d", partCount)
	}
	if !strings.Contains(attDisp, "test.pdf") {
		t.Errorf("attachment part Content-Disposition: got %q, want filename=test.pdf", attDisp)
	}
}

func TestBuildMIMEMessage_HTMLWithAttachments(t *testing.T) {
	// HTML body + attachment should produce multipart/mixed with a
	// multipart/alternative inner part and an attachment part.
	att := OutgoingAttachment{
		Filename:    "image.png",
		ContentType: "image/png",
		Data:        []byte("\x89PNG fake"),
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:        "alice@example.com",
		To:          "bob@example.com",
		Subject:     "Rich with attachment",
		PlainBody:   "Plain text.",
		HTMLBody:    "<p>HTML text.</p>",
		Attachments: []OutgoingAttachment{att},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	outerCT, outerParams, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse outer content-type: %v", err)
	}
	if outerCT != "multipart/mixed" {
		t.Errorf("outer Content-Type: got %q, want multipart/mixed", outerCT)
	}

	mr := multipart.NewReader(msg.Body, outerParams["boundary"])
	p1, err := mr.NextPart()
	if err != nil {
		t.Fatal("next part (body): ", err)
	}
	innerCT, _, err := mime.ParseMediaType(p1.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse inner content-type: %v", err)
	}
	if innerCT != "multipart/alternative" {
		t.Errorf("inner body Content-Type: got %q, want multipart/alternative", innerCT)
	}

	p2, err := mr.NextPart()
	if err != nil {
		t.Fatal("next part (attachment): ", err)
	}
	if !strings.Contains(p2.Header.Get("Content-Disposition"), "image.png") {
		t.Errorf("attachment part missing filename: %q", p2.Header.Get("Content-Disposition"))
	}
}

func TestBuildMIMEMessage_ThreadingHeaders(t *testing.T) {
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:       "alice@example.com",
		To:         "bob@example.com",
		Subject:    "Re: Hello",
		PlainBody:  "Reply body.",
		InReplyTo:  "<abc123@example.com>",
		References: "<orig@example.com> <abc123@example.com>",
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	if got := msg.Header.Get("In-Reply-To"); got != "<abc123@example.com>" {
		t.Errorf("In-Reply-To: got %q", got)
	}
	if got := msg.Header.Get("References"); !strings.Contains(got, "<orig@example.com>") {
		t.Errorf("References: got %q", got)
	}
}

func TestBuildMIMEMessage_EmptyBody(t *testing.T) {
	_, err := BuildMIMEMessage(MIMEMessageOptions{
		From:    "alice@example.com",
		To:      "bob@example.com",
		Subject: "Empty",
	})
	if err == nil {
		t.Error("expected error for empty body, got nil")
	}
}

func TestParseAddressField(t *testing.T) {
	tests := []struct {
		input string
		want  []RecipientEntry
	}{
		{
			"alice@example.com",
			[]RecipientEntry{{Email: "alice@example.com"}},
		},
		{
			"Alice Smith <alice@example.com>, bob@example.com",
			[]RecipientEntry{
				{Email: "alice@example.com", Name: "Alice Smith"},
				{Email: "bob@example.com"},
			},
		},
		{
			"",
			nil,
		},
	}
	for _, tt := range tests {
		got := ParseAddressField(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("ParseAddressField(%q): got %d results, want %d", tt.input, len(got), len(tt.want))
			continue
		}
		for i, g := range got {
			w := tt.want[i]
			if g.Email != w.Email || g.Name != w.Name {
				t.Errorf("result[%d]: got {%q,%q}, want {%q,%q}", i, g.Email, g.Name, w.Email, w.Name)
			}
		}
	}
}

func TestStripHTMLForPlain(t *testing.T) {
	// Use the exported-like helper via the web package — but since it's in the
	// web package we test the stripHTMLForPlain equivalent (stripHTML is in api).
	// Test stripHTML which is already in email.go (same package).
	html := "<h1>Hello</h1><p>World &amp; friends</p>"
	got := stripHTML(html)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("stripHTML left tags: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
		t.Errorf("stripHTML lost content: %q", got)
	}
}
