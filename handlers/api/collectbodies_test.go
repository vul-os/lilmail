package api

import (
	"strings"
	"testing"

	"lilmail/models"
)

// collect is a helper mirroring how processMessage invokes collectBodies on a
// parsed net/mail message: top-level headers, then the raw body reader.
func collect(t *testing.T, contentType, cte, disposition, body string) models.Email {
	t.Helper()
	var email models.Email
	collectBodies(strings.NewReader(body), contentType, cte, disposition, &email, 0)
	return email
}

// A single-part text/html message — the exact shape of Anthropic's
// "Secure link to log in to Claude.ai" mail — must land in HTML, not Body.
// The viewer template renders HTML in an iframe and only falls back to
// linkified plain text, so misfiling this shows the reader raw HTML source.
func TestSinglePartHTMLQuotedPrintable(t *testing.T) {
	got := collect(t, "text/html; charset=us-ascii", "quoted-printable", "",
		"<p>Click =E2=80=94 <a href=3D\"https://claude.ai/magic-link\">here</a>=\r\n to log in</p>")

	if got.Body != "" {
		t.Errorf("Body should stay empty for a text/html message, got %q", got.Body)
	}
	want := `<p>Click — <a href="https://claude.ai/magic-link">here</a> to log in</p>`
	if got.HTML != want {
		t.Errorf("HTML not transfer-decoded\n got: %q\nwant: %q", got.HTML, want)
	}
}

func TestSinglePartPlainBase64(t *testing.T) {
	// base64("我们将于 2026 年") — wrapped, as transports emit it.
	got := collect(t, "text/plain; charset=utf-8", "base64", "",
		"5oiR5Lus5bCG5LqOIDIwMjYg5bm0\r\n")
	if got.HTML != "" {
		t.Errorf("HTML should stay empty, got %q", got.HTML)
	}
	if got.Body != "我们将于 2026 年" {
		t.Errorf("base64 body not decoded: %q", got.Body)
	}
}

func TestMultipartAlternativeBase64Plain(t *testing.T) {
	body := "--b1\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"aGVsbG8gcGxhaW4=\r\n" +
		"--b1\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<b>hello html</b>\r\n" +
		"--b1--\r\n"
	got := collect(t, `multipart/alternative; boundary="b1"`, "", "", body)

	if got.Body != "hello plain" {
		t.Errorf("base64 part not decoded: %q", got.Body)
	}
	if !strings.Contains(got.HTML, "<b>hello html</b>") {
		t.Errorf("html part missing: %q", got.HTML)
	}
}

// multipart/mixed › multipart/alternative — the old code only looked one level
// deep, so the nested container matched neither text/plain nor text/html and
// the message rendered blank.
func TestNestedMultipartRecursion(t *testing.T) {
	body := "--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"nested plain\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<i>nested html</i>\r\n" +
		"--inner--\r\n" +
		"--outer--\r\n"
	got := collect(t, `multipart/mixed; boundary="outer"`, "", "", body)

	if !strings.Contains(got.Body, "nested plain") {
		t.Errorf("nested plain not found: %q", got.Body)
	}
	if !strings.Contains(got.HTML, "<i>nested html</i>") {
		t.Errorf("nested html not found: %q", got.HTML)
	}
}

// A text/plain attachment must not become the message body.
func TestAttachmentDoesNotClobberBody(t *testing.T) {
	body := "--b2\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Disposition: attachment; filename=\"notes.txt\"\r\n\r\n" +
		"ATTACHED FILE CONTENT\r\n" +
		"--b2\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"the real body\r\n" +
		"--b2--\r\n"
	got := collect(t, `multipart/mixed; boundary="b2"`, "", "", body)

	if strings.Contains(got.Body, "ATTACHED") {
		t.Errorf("attachment leaked into body: %q", got.Body)
	}
	if !strings.Contains(got.Body, "the real body") {
		t.Errorf("real body lost: %q", got.Body)
	}
}

// The list preview slices the first 512 bytes of a body, so decoding must
// survive input cut mid-quantum instead of falling back to raw gibberish.
func TestDecodeTransferBytesToleratesTruncation(t *testing.T) {
	full := "aGVsbG8gd29ybGQsIHRoaXMgaXMgYSBsb25nZXIgbGluZQ==" // "hello world, this is a longer line"
	truncated := full[:20]                                     // not a multiple of 4
	got := string(decodeTransferBytes([]byte(truncated), "base64"))
	if !strings.HasPrefix(got, "hello world") {
		t.Errorf("truncated base64 should decode its clean prefix, got %q", got)
	}

	qp := "caf=C3=A9 and =E2=80" // trailing escape cut in half
	got = string(decodeTransferBytes([]byte(qp), "quoted-printable"))
	if !strings.HasPrefix(got, "café") {
		t.Errorf("truncated quoted-printable should keep its prefix, got %q", got)
	}
}

// A marketing mail's preview must not be its CSS reset. This is the literal
// text Anthropic's and Tripo's mail produced in the message list.
func TestStripHTMLSkipsStyleAndScript(t *testing.T) {
	markup := `<head><style type="text/css">#outlook a { padding:0; } body { margin:0; }</style></head>` +
		`<body><script>var x = 1 < 2;</script><p>Your login link is ready</p></body>`
	got := stripHTML(markup)

	if strings.Contains(got, "#outlook") || strings.Contains(got, "padding") {
		t.Errorf("CSS leaked into preview: %q", got)
	}
	if strings.Contains(got, "var x") {
		t.Errorf("script leaked into preview: %q", got)
	}
	if !strings.Contains(got, "Your login link is ready") {
		t.Errorf("real text lost: %q", got)
	}
}

func TestStripHTMLEntitiesAndZeroWidth(t *testing.T) {
	got := stripHTML("<p>Caf&eacute; &amp; bar\u200c\u200b\ufeff</p>")
	if got != "Café & bar" {
		t.Errorf("entities/zero-width not handled: %q", got)
	}
}

// <styles> is not a <style> element; its text must survive.
func TestStripHTMLDoesNotEatLookalikeTag(t *testing.T) {
	got := stripHTML("<styles-x>kept</styles-x>")
	if got != "kept" {
		t.Errorf("lookalike tag ate content: %q", got)
	}
}

// Outlook conditional comments carry markup, not prose: their <o:PixelsPerInch>
// value was leaking into previews as a stray "96".
func TestStripHTMLSkipsComments(t *testing.T) {
	markup := `<!--[if mso]><xml><o:OfficeDocumentSettings><o:PixelsPerInch>96` +
		`</o:PixelsPerInch></o:OfficeDocumentSettings></xml><![endif]-->` +
		`<p>Let's get you signed in</p>`
	got := stripHTML(markup)
	if strings.Contains(got, "96") {
		t.Errorf("conditional comment leaked into preview: %q", got)
	}
	if !strings.HasPrefix(got, "Let's get you signed in") {
		t.Errorf("real text lost or displaced: %q", got)
	}
}

// The 4096-byte preview window overruns the first part of a multipart message
// into the next MIME boundary; a base64 part must still decode up to that point
// instead of failing whole and showing the reader raw base64.
func TestDecodeBase64StopsAtMIMEBoundary(t *testing.T) {
	// base64("我们将于 2026 年") followed by the next boundary and part headers.
	chunk := "5oiR5Lus5bCG5LqOIDIwMjYg5bm0\r\n--b1\r\nContent-Type: text/html\r\n"
	got := string(decodeTransferBytes([]byte(chunk), "base64"))
	if !strings.HasPrefix(got, "我们将于 2026 年") {
		t.Errorf("base64 prefix not decoded before boundary: %q", got)
	}
	if strings.Contains(got, "Content-Type") {
		t.Errorf("boundary headers leaked into decoded text: %q", got)
	}
}
