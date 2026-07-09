// handlers/api/unsubscribe.go — parse the List-Unsubscribe (RFC 2369) and
// List-Unsubscribe-Post (RFC 8058) headers into the distilled targets surfaced
// on models.Email.Unsubscribe.
//
// Bulk senders advertise how to unsubscribe with:
//
//	List-Unsubscribe: <https://example.com/u/abc>, <mailto:unsub@example.com?subject=unsub>
//	List-Unsubscribe-Post: List-Unsubscribe=One-Click
//
// The second header (RFC 8058) opts the sender into a one-click HTTPS POST flow:
// the mail client may POST the body `List-Unsubscribe=One-Click` to the https URI
// WITHOUT any user interaction beyond a single click, and WITHOUT following any
// link. lilmail parses these read-only; it NEVER itself unsubscribes and never
// dereferences the URL. The client decides, confirms, and acts.
//
// SECURITY: only http/https and mailto: schemes are surfaced. Any other scheme
// (javascript:, data:, file:, ftp:, …) is dropped so a hostile scheme can never
// reach the client. The one-click flag is reported only when BOTH the RFC 8058
// header is present AND an https(!) target exists — a plain-http one-click POST
// would leak the token in cleartext, so we require TLS for auto-POST.
package api

import (
	"strings"

	"lilmail/models"
)

// ParseUnsubscribe distills the List-Unsubscribe / List-Unsubscribe-Post header
// values into an *models.Unsubscribe. Returns nil when there is no usable target
// (no header, or only unsupported schemes), so an ordinary message simply has
// Email.Unsubscribe == nil (no button).
//
// listUnsub is every List-Unsubscribe value on the message (RFC allows folding /
// multiple), listUnsubPost is every List-Unsubscribe-Post value.
func ParseUnsubscribe(listUnsub, listUnsubPost []string) *models.Unsubscribe {
	var httpURL, mailtoURL string
	for _, raw := range listUnsub {
		for _, uri := range extractBracketedURIs(raw) {
			lower := strings.ToLower(uri)
			switch {
			case httpURL == "" && (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")):
				httpURL = uri
			case mailtoURL == "" && strings.HasPrefix(lower, "mailto:"):
				mailtoURL = uri
			}
			// Everything else (javascript:, data:, file:, ftp:, …) is ignored.
		}
	}

	if httpURL == "" && mailtoURL == "" {
		return nil
	}

	// RFC 8058 one-click requires the POST header AND an https target (we refuse
	// to auto-POST an unsubscribe token over plaintext http). A message that only
	// advertises http or mailto is still surfaced, but without the one-click flag,
	// so the client opens it in a new tab / mail window instead of auto-POSTing.
	oneClick := false
	if httpURL != "" && strings.HasPrefix(strings.ToLower(httpURL), "https://") && hasOneClickPost(listUnsubPost) {
		oneClick = true
	}

	return &models.Unsubscribe{
		HTTPURL:   httpURL,
		MailtoURL: mailtoURL,
		OneClick:  oneClick,
	}
}

// extractBracketedURIs pulls each `<uri>` out of a List-Unsubscribe value.
// Per RFC 2369 every URI is angle-bracketed and comma-separated; we scan for
// the bracket pairs rather than splitting on commas (a mailto: query can itself
// contain a comma). Whitespace inside brackets is trimmed. Malformed input
// (unbalanced brackets) yields nothing for that fragment — fail-closed.
func extractBracketedURIs(s string) []string {
	var out []string
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			break
		}
		close := strings.IndexByte(s[open+1:], '>')
		if close < 0 {
			break
		}
		uri := strings.TrimSpace(s[open+1 : open+1+close])
		if uri != "" {
			out = append(out, uri)
		}
		s = s[open+1+close+1:]
	}
	return out
}

// hasOneClickPost reports whether any List-Unsubscribe-Post header advertises
// RFC 8058 one-click: the token `List-Unsubscribe=One-Click` (case-insensitive).
func hasOneClickPost(vals []string) bool {
	for _, v := range vals {
		if strings.Contains(strings.ToLower(strings.TrimSpace(v)), "list-unsubscribe=one-click") {
			return true
		}
	}
	return false
}
