// handlers/api/mime_builder_test.go — tests for MIME message assembly.
package api

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
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

// walkParts recursively descends a MIME tree, invoking fn for every leaf and
// non-leaf part with its parsed media type, params and header. Multipart parts
// are recursed into; leaf bodies are read and passed to fn.
func walkParts(t *testing.T, mediaType string, params map[string]string, hdr textproto.MIMEHeader, body []byte, fn func(mt string, params map[string]string, hdr textproto.MIMEHeader, body []byte)) {
	t.Helper()
	fn(mediaType, params, hdr, body)
	if !strings.HasPrefix(mediaType, "multipart/") {
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		pb, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		pmt, pparams, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse part content-type %q: %v", p.Header.Get("Content-Type"), err)
		}
		walkParts(t, pmt, pparams, p.Header, pb, fn)
	}
}

// collectParts returns every part in the tree as a flat slice for assertions.
type mimePart struct {
	mediaType string
	params    map[string]string
	hdr       textproto.MIMEHeader
	body      []byte
}

func collectParts(t *testing.T, raw []byte) []mimePart {
	t.Helper()
	msg := parseMail(t, raw)
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse top content-type: %v", err)
	}
	topHdr := textproto.MIMEHeader{}
	for k, v := range msg.Header {
		topHdr[k] = v
	}
	var parts []mimePart
	walkParts(t, mt, params, topHdr, body, func(m string, p map[string]string, h textproto.MIMEHeader, b []byte) {
		parts = append(parts, mimePart{mediaType: m, params: p, hdr: h, body: b})
	})
	return parts
}

func findPart(parts []mimePart, mediaType string) *mimePart {
	for i := range parts {
		if parts[i].mediaType == mediaType {
			return &parts[i]
		}
	}
	return nil
}

func TestBuildMIMEMessage_InlineImageOnly(t *testing.T) {
	// HTML body + one inline (cid) image, no regular attachments →
	// top-level multipart/related whose root is multipart/alternative and whose
	// second part is the inline image with the right Content-ID + disposition.
	inline := OutgoingAttachment{
		Filename:    "logo.png",
		ContentType: "image/png",
		Data:        []byte("\x89PNG inline bytes"),
		ContentID:   "logo123",
		Inline:      true,
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:        "alice@example.com",
		To:          "bob@example.com",
		Subject:     "Inline image",
		PlainBody:   "Plain fallback.",
		HTMLBody:    `<p>See <img src="cid:logo123"></p>`,
		Attachments: []OutgoingAttachment{inline},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	topMT, _, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse top: %v", err)
	}
	if topMT != "multipart/related" {
		t.Fatalf("top Content-Type: got %q, want multipart/related", topMT)
	}

	parts := collectParts(t, raw)
	// Must contain a multipart/alternative (the root body) nested in the related.
	if findPart(parts, "multipart/alternative") == nil {
		t.Error("expected a multipart/alternative root inside related")
	}
	// Must contain the inline image with Content-ID <logo123> and inline disp.
	img := findPart(parts, "image/png")
	if img == nil {
		t.Fatal("inline image/png part not found")
	}
	if got := img.hdr.Get("Content-ID"); got != "<logo123>" {
		t.Errorf("inline Content-ID: got %q, want <logo123>", got)
	}
	if got := img.hdr.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("inline Content-Disposition: got %q, want inline...", got)
	}
	if got := img.hdr.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Errorf("inline CTE: got %q, want base64", got)
	}
}

func TestBuildMIMEMessage_InlineAndRegularAttachment(t *testing.T) {
	// Inline image + a regular attachment → outer multipart/mixed containing a
	// multipart/related (body + inline) and the regular attachment.
	inline := OutgoingAttachment{
		Filename: "sig.png", ContentType: "image/png",
		Data: []byte("sig bytes"), ContentID: "sig@vulos", Inline: true,
	}
	regular := OutgoingAttachment{
		Filename: "report.pdf", ContentType: "application/pdf",
		Data: []byte("%PDF fake"),
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:        "alice@example.com",
		To:          "bob@example.com",
		Subject:     "Inline + attachment",
		PlainBody:   "Plain.",
		HTMLBody:    `<p><img src="cid:sig@vulos"></p>`,
		Attachments: []OutgoingAttachment{inline, regular},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}

	msg := parseMail(t, raw)
	topMT, _, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse top: %v", err)
	}
	if topMT != "multipart/mixed" {
		t.Fatalf("top Content-Type: got %q, want multipart/mixed", topMT)
	}

	parts := collectParts(t, raw)
	if findPart(parts, "multipart/related") == nil {
		t.Error("expected a multipart/related nested in mixed")
	}
	// Inline image present with its Content-ID.
	img := findPart(parts, "image/png")
	if img == nil || img.hdr.Get("Content-ID") != "<sig@vulos>" {
		t.Errorf("inline image/png with Content-ID <sig@vulos> not found: %+v", img)
	}
	// Regular attachment present with attachment disposition + filename.
	pdf := findPart(parts, "application/pdf")
	if pdf == nil {
		t.Fatal("regular application/pdf attachment not found")
	}
	if d := pdf.hdr.Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") || !strings.Contains(d, "report.pdf") {
		t.Errorf("regular attachment disposition: got %q", d)
	}
	// The regular attachment must NOT carry a Content-ID.
	if pdf.hdr.Get("Content-ID") != "" {
		t.Errorf("regular attachment unexpectedly has Content-ID %q", pdf.hdr.Get("Content-ID"))
	}
}

func TestBuildMIMEMessage_InlineWithoutHTMLDegradesToAttachment(t *testing.T) {
	// An inline flag with no HTML body has nothing to reference; it must degrade
	// to a regular attachment (multipart/mixed, no related, no Content-ID).
	inline := OutgoingAttachment{
		Filename: "orphan.png", ContentType: "image/png",
		Data: []byte("png"), ContentID: "orphan", Inline: true,
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From:        "alice@example.com",
		To:          "bob@example.com",
		Subject:     "Orphan inline",
		PlainBody:   "Just text, no HTML.",
		Attachments: []OutgoingAttachment{inline},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}
	msg := parseMail(t, raw)
	topMT, _, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse top: %v", err)
	}
	if topMT != "multipart/mixed" {
		t.Errorf("top: got %q, want multipart/mixed (degraded)", topMT)
	}
	parts := collectParts(t, raw)
	if findPart(parts, "multipart/related") != nil {
		t.Error("did not expect multipart/related when there is no HTML body")
	}
	img := findPart(parts, "image/png")
	if img == nil {
		t.Fatal("image part not found")
	}
	if img.hdr.Get("Content-ID") != "" {
		t.Errorf("degraded attachment should have no Content-ID, got %q", img.hdr.Get("Content-ID"))
	}
	if d := img.hdr.Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") {
		t.Errorf("degraded disposition: got %q, want attachment", d)
	}
}

func TestBuildMIMEMessage_NoInlineUnchanged(t *testing.T) {
	// Regression: a message with only a regular attachment and HTML body must
	// still be multipart/mixed( multipart/alternative, attachment ) — no related.
	att := OutgoingAttachment{
		Filename: "file.txt", ContentType: "text/plain", Data: []byte("hi"),
	}
	raw, err := BuildMIMEMessage(MIMEMessageOptions{
		From: "a@example.com", To: "b@example.com", Subject: "Reg",
		PlainBody: "p", HTMLBody: "<p>h</p>",
		Attachments: []OutgoingAttachment{att},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage: %v", err)
	}
	parts := collectParts(t, raw)
	if findPart(parts, "multipart/related") != nil {
		t.Error("unexpected multipart/related in a no-inline message")
	}
	if findPart(parts, "multipart/mixed") == nil {
		t.Error("expected multipart/mixed")
	}
	if findPart(parts, "multipart/alternative") == nil {
		t.Error("expected multipart/alternative")
	}
}

func TestValidateContentID(t *testing.T) {
	ok := []string{"logo123", "sig@vulos.com", "a.b_c-d+e%f", "x@1.2.3"}
	for _, id := range ok {
		if err := validateContentID(id); err != nil {
			t.Errorf("validateContentID(%q): unexpected error %v", id, err)
		}
	}
	bad := []string{
		"",                        // empty
		"has space",               // whitespace
		"line\r\nContent-Type: x", // CRLF header injection
		"line\ninjected",          // LF injection
		"has<angle",               // could break the <...> wrapper
		"has>angle",
		"quote\"here",
		strings.Repeat("a", 256), // too long
	}
	for _, id := range bad {
		if err := validateContentID(id); err == nil {
			t.Errorf("validateContentID(%q): expected error, got nil", id)
		}
	}
}

func TestBuildMIMEMessage_InlineContentIDInjectionRejected(t *testing.T) {
	// A Content-ID carrying a CRLF header-injection payload must be rejected at
	// build time rather than emitted into the header stream.
	evil := OutgoingAttachment{
		Filename: "x.png", ContentType: "image/png", Data: []byte("png"),
		ContentID: "abc\r\nBcc: attacker@evil.example", Inline: true,
	}
	_, err := BuildMIMEMessage(MIMEMessageOptions{
		From: "a@example.com", To: "b@example.com", Subject: "Evil",
		PlainBody: "p", HTMLBody: `<img src="cid:abc">`,
		Attachments: []OutgoingAttachment{evil},
	})
	if err == nil {
		t.Fatal("expected error for injected Content-ID, got nil")
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
