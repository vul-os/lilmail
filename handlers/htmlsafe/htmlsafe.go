// Package htmlsafe is a conservative, dependency-free HTML sanitizer for
// attacker-influenced HTML that lilmail must render or emit outside the
// sandboxed reading-pane iframe.
//
// Two callers share this policy:
//
//   - handlers/jsonapi — defangs user-authored signature/vacation HTML that
//     lilmail EMITS into outgoing mail (defense-in-depth; the recipient's client
//     also sanitizes).
//   - handlers/web — defangs the HTML of a DRAFT being edited before it is placed
//     into the compose contenteditable. That contenteditable is assigned via
//     `innerHTML` in the MAIN app document (see
//     templates/partials/email-viewer.html restoreDraft()), which — unlike the
//     reading-pane iframe — is NOT sandboxed, so raw mail HTML there is a stored
//     XSS vector. `innerHTML` fires load/error handlers on inserted nodes
//     (<img src=x onerror=...>, <svg onload=...>), so stripping those handlers
//     and the active/structural elements server-side is what closes the hole.
//
// WHY A REGEX ALLOWLIST/DENYLIST (no new dependency): the goal is to neutralise
// the active constructs (script, event handlers, javascript:/vbscript:/data:
// URLs, <style>, <iframe>, <svg>, forms) while preserving benign formatting
// (<b>/<a href="https://...">/<p>/lists/<img>). This is intentionally BLUNT: it
// removes dangerous constructs rather than parsing+rebuilding a DOM. A
// false-positive strip on exotic markup is an acceptable trade for not shipping a
// new HTML-parsing dependency.
package htmlsafe

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
	// Inline event handlers: on*="..." / on*='...' / on*=bare. The attribute
	// separator preceding the handler may be ANY of whitespace OR "/" — HTML treats
	// "<a/onclick=...>" identically to "<a onclick=...>", so anchoring only on \s
	// (the wave-56 original) let a slash-separated handler slip through. We now
	// accept [\s/] and REQUIRE the "on<word>=" shape so ordinary text/paths like
	// "/online" or href="/onboarding" (no following "=") are not touched.
	reOnEvent = regexp.MustCompile(`(?is)[\s/]on[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// Dangerous URL schemes anywhere a URL can appear (href/src/etc.). We neutralise
	// the scheme token rather than the whole attribute so surrounding markup survives.
	// A browser STRIPS tab/newline/CR/FF (but NOT the space char) from within a URL
	// before resolving the scheme, so an attacker can split "java<TAB>script:" (or
	// reach it via a decoded numeric entity like &#x0A;). We tolerate ONLY those
	// stripped control chars BETWEEN the scheme letters — not the space char — so
	// ordinary prose like "the Java script:" is not falsely rewritten.
	reJSScheme   = regexp.MustCompile(`(?is)j[\t\n\r\f]*a[\t\n\r\f]*v[\t\n\r\f]*a[\t\n\r\f]*s[\t\n\r\f]*c[\t\n\r\f]*r[\t\n\r\f]*i[\t\n\r\f]*p[\t\n\r\f]*t[\t\n\r\f]*:|v[\t\n\r\f]*b[\t\n\r\f]*s[\t\n\r\f]*c[\t\n\r\f]*r[\t\n\r\f]*i[\t\n\r\f]*p[\t\n\r\f]*t[\t\n\r\f]*:`)
	reDataScheme = regexp.MustCompile(`(?is)(href|src|action|formaction)\s*=\s*(["']?)\s*data\s*:`)
	// Leftover <script>/<style> with no close tag (truncated payloads).
	reOpenScript = regexp.MustCompile(`(?is)<\s*/?\s*(script|style)\b[^>]*>?`)
	// HTML numeric character references (decimal &#106; or hex &#x6a;, optionally
	// unterminated). A browser DECODES these inside an attribute value BEFORE it
	// evaluates the URL scheme, so "href=&#106;avascript:..." reconstitutes
	// "javascript:" after our reJSScheme pass has already run. We decode numeric
	// references to their character FIRST so the scheme neutraliser sees the real
	// scheme. (Named entities like &lt; are left intact — they cannot form a
	// scheme keyword and decoding them could re-introduce markup.)
	reNumEntity = regexp.MustCompile(`&#(x[0-9a-fA-F]+|[0-9]+);?`)
)

// maxSnippetBytes bounds a single sanitized body so a caller cannot park an
// unbounded blob in durable storage (a cheap DoS), bloat every outgoing message,
// or blow up the regex engine. Generous for a rich HTML signature/draft with an
// inline logo referenced by URL (not embedded).
const maxSnippetBytes = 64 * 1024

// SanitizeSnippet returns a defanged copy of attacker-influenced HTML:
// script/style elements removed, active/structural elements stripped, inline
// event handlers removed, and javascript:/vbscript:/data: URLs neutralised. It
// enforces a size bound (over-length input is truncated before sanitizing, so a
// giant blob cannot be used to blow up the regex engine). Benign formatting
// (<b>, <a href="https://...">, <p>, lists, <img>) is preserved.
func SanitizeSnippet(in string) string {
	if in == "" {
		return ""
	}
	if len(in) > maxSnippetBytes {
		in = in[:maxSnippetBytes]
	}
	out := in
	// Decode numeric character references FIRST so an entity-obfuscated scheme
	// (e.g. "&#106;avascript:") is un-hidden before the scheme neutralisers run.
	out = decodeDangerousNumEntities(out)
	out = reScriptEl.ReplaceAllString(out, "")
	out = reStyleEl.ReplaceAllString(out, "")
	out = reDangerTag.ReplaceAllString(out, "")
	out = reOnEvent.ReplaceAllString(out, "")
	out = reDataScheme.ReplaceAllString(out, "$1=$2")  // drop the "data:" scheme, keep the attr harmless/empty
	out = reJSScheme.ReplaceAllString(out, "blocked:") // neutralise javascript:/vbscript:
	out = reOpenScript.ReplaceAllString(out, "")       // sweep any truncated script/style tag
	return strings.TrimSpace(out)
}

// decodeDangerousNumEntities decodes HTML numeric character references (decimal or
// hex) that map to ASCII letters, ":" or whitespace — the only characters an
// attacker needs to smuggle a "javascript:"/"vbscript:"/"data:" scheme past the
// scheme neutraliser. Everything else (e.g. &#60; which is "<") is left ENCODED so
// decoding cannot re-introduce live markup. After this pass the scheme regexes see
// the real scheme text and can neutralise it.
func decodeDangerousNumEntities(s string) string {
	if !strings.Contains(s, "&#") {
		return s
	}
	return reNumEntity.ReplaceAllStringFunc(s, func(m string) string {
		// Strip "&#" prefix and optional ";" suffix, then parse dec/hex.
		body := strings.TrimSuffix(strings.TrimPrefix(m, "&#"), ";")
		var cp int
		if len(body) > 0 && (body[0] == 'x' || body[0] == 'X') {
			for _, r := range body[1:] {
				d := hexVal(r)
				if d < 0 {
					return m // malformed → leave as-is
				}
				cp = cp*16 + d
				if cp > 0x10FFFF {
					return m
				}
			}
		} else {
			for _, r := range body {
				if r < '0' || r > '9' {
					return m
				}
				cp = cp*10 + int(r-'0')
				if cp > 0x10FFFF {
					return m
				}
			}
		}
		// Only decode the narrow set of characters useful for scheme smuggling.
		switch {
		case cp >= 'a' && cp <= 'z', cp >= 'A' && cp <= 'Z':
			return string(rune(cp))
		case cp == ':' || cp == '\t' || cp == '\n' || cp == '\r' || cp == ' ':
			return string(rune(cp))
		default:
			return m // leave anything else (esp. "<", ">", "&") encoded
		}
	})
}

func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	default:
		return -1
	}
}
