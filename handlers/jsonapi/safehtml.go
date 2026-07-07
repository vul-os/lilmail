// handlers/jsonapi/safehtml.go — conservative HTML sanitizer for user-authored
// snippets that lilmail EMITS into outgoing mail: signatures and vacation-reply
// bodies.
//
// WHY A REGEX ALLOWLIST (no new dependency): these snippets are not rendered in
// the lilmail app DOM — they are composed into an outgoing MIME text/html part
// and read by the RECIPIENT's mail client, which already applies its own strict
// mail-HTML sanitization. Our job here is defense-in-depth: strip the active
// content that has no place in a signature/auto-reply (script, event handlers,
// javascript:/data: URLs, <style>, <iframe>, forms) so a stored XSS payload
// cannot ride out on our envelope, and so the mail-ui can safely preview the
// snippet. This is intentionally BLUNT: it removes dangerous constructs rather
// than trying to parse+rebuild a DOM. A false-positive strip on an exotic
// signature is an acceptable trade for not shipping a new HTML-parsing dep.
//
// SEPARATELY, validateHeaderValue (wave-49) still guards every HEADER these
// features touch (vacation Subject, send-as From) against CR/LF/NUL injection —
// this file guards the BODY.
package jsonapi

import (
	"regexp"
	"strings"
)

var (
	// Whole dangerous elements incl. their content.
	reScriptEl = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	reStyleEl  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`)
	// Structural/active elements we drop entirely (tags only; content, if any, kept).
	reDangerTag = regexp.MustCompile(`(?is)</?(?:iframe|object|embed|form|link|meta|base|frame|frameset|applet|svg|math)\b[^>]*>`)
	// Inline event handlers: on*="..." / on*='...' / on*=bare.
	reOnEvent = regexp.MustCompile(`(?is)\son[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// Dangerous URL schemes anywhere a URL can appear (href/src/etc.). We neutralise
	// the scheme token rather than the whole attribute so surrounding markup survives.
	reJSScheme   = regexp.MustCompile(`(?is)(javascript|vbscript)\s*:`)
	reDataScheme = regexp.MustCompile(`(?is)(href|src|action|formaction)\s*=\s*(["']?)\s*data\s*:`)
	// Leftover <script>/<style> with no close tag (truncated payloads).
	reOpenScript = regexp.MustCompile(`(?is)<\s*/?\s*(script|style)\b[^>]*>?`)
)

// maxSignatureBytes bounds a single stored signature/vacation body so a caller
// cannot park an unbounded blob in durable storage (a cheap DoS) or bloat every
// outgoing message. Generous for a rich HTML signature with an inline logo
// referenced by URL (not embedded).
const maxSignatureBytes = 64 * 1024

// sanitizeSnippetHTML returns a defanged copy of user-authored signature/vacation
// HTML: script/style elements removed, active/structural elements stripped, inline
// event handlers removed, and javascript:/vbscript:/data: URLs neutralised. Also
// enforces the size bound (over-length input is truncated before sanitizing, so a
// giant blob cannot be used to blow up the regex engine).
func sanitizeSnippetHTML(in string) string {
	if in == "" {
		return ""
	}
	if len(in) > maxSignatureBytes {
		in = in[:maxSignatureBytes]
	}
	out := in
	out = reScriptEl.ReplaceAllString(out, "")
	out = reStyleEl.ReplaceAllString(out, "")
	out = reDangerTag.ReplaceAllString(out, "")
	out = reOnEvent.ReplaceAllString(out, "")
	out = reDataScheme.ReplaceAllString(out, "$1=$2")  // drop the "data:" scheme, keep the attr harmless/empty
	out = reJSScheme.ReplaceAllString(out, "blocked:") // neutralise javascript:/vbscript:
	out = reOpenScript.ReplaceAllString(out, "")       // sweep any truncated script/style tag
	return strings.TrimSpace(out)
}
