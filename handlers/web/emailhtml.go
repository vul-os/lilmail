// handlers/web/emailhtml.go
//
// Helpers that turn a raw HTML email body into a document suitable for
// rendering inside the reading-pane iframe.
//
// Sizing / sandboxing approach
// ----------------------------
// The reading pane renders HTML mail in an <iframe srcdoc="...">. We want the
// iframe to grow to the natural height of its content (Gmail-style) so the
// whole reading pane scrolls as one document instead of trapping the message in
// a small inner scroll box.
//
// To measure content height the *parent* page reads
// iframe.contentDocument.body.scrollHeight. That requires the framed document
// to share the parent origin, i.e. the iframe needs sandbox="allow-same-origin"
// — but crucially WITHOUT allow-scripts. With scripts disabled the email cannot
// run any JavaScript, so granting same-origin is safe: there is no script in
// the frame that could reach back into the parent. (The dangerous combination
// is allow-same-origin together WITH allow-scripts, which lets framed content
// remove its own sandbox — we never grant both.)
//
// Privacy / remote images
// -----------------------
// Remote images and CSS backgrounds are neutralised server-side before the HTML
// ever reaches the browser, so no tracking pixel or remote asset loads until the
// user explicitly clicks "Display images". Original URLs are stashed in
// data-blocked-* attributes; the parent page restores them on demand (it can,
// because the frame is same-origin and script-free).
package web

import (
	"regexp"
	"strings"
)

// transparent 1x1 GIF — used as the placeholder for blocked remote images so
// layout/dimensions are preserved without leaking a request.
const blockedImgPlaceholder = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

var (
	imgTagRe    = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	srcAttrRe   = regexp.MustCompile(`(?is)\bsrc\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	srcsetRe    = regexp.MustCompile(`(?is)\bsrcset\s*=\s*("[^"]*"|'[^']*')`)
	bgAttrRe    = regexp.MustCompile(`(?is)\bbackground\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	styleAttrRe = regexp.MustCompile(`(?is)style\s*=\s*("[^"]*"|'[^']*')`)
	cssURLRe    = regexp.MustCompile(`(?is)url\(\s*['"]?\s*(https?:|//)`)
)

// emailFrameCSS is injected into every rendered HTML email so messages get a
// consistent, readable baseline (typography, sensible image sizing, link colour,
// table overflow handling) regardless of how the original mail was authored.
const emailFrameCSS = `
:root { color-scheme: light dark; }
html, body { margin: 0; padding: 0; }
body {
  padding: 18px 22px;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  font-size: 15px;
  line-height: 1.6;
  color: #1f2a37;
  background: #ffffff;
  -webkit-text-size-adjust: 100%;
  text-size-adjust: 100%;
  word-wrap: break-word;
  overflow-wrap: break-word;
}
img, video { max-width: 100%; height: auto; }
/* Blocked remote images render as a slim dashed placeholder instead of a giant
   square (the 1x1 transparent GIF would otherwise inherit the width attribute's
   aspect ratio). Restored to natural size once the user clicks "Display images". */
img[data-blocked-src] {
  height: 34px !important;
  min-width: 34px;
  max-width: 100%;
  background: #f0e9e2;
  border: 1px dashed #c9bfb4;
  border-radius: 4px;
}
@media (prefers-color-scheme: dark) {
  img[data-blocked-src] { background: #2a251f; border-color: #4a4239; }
}
table { max-width: 100%; }
a { color: #0e9384; }
pre, code { white-space: pre-wrap; word-break: break-word; font-family: ui-monospace, "SF Mono", Consolas, monospace; }
blockquote { border-left: 3px solid #d7d2cc; margin: 12px 0; padding-left: 14px; color: #6b7280; }
/* Keep wide content scrollable rather than blowing out the frame width. */
* { max-width: 100%; }
@media (prefers-color-scheme: dark) {
  body { color: #f4efe9; background: #211d18; }
  a { color: #5eead9; }
  blockquote { border-left-color: #3a332b; color: #b7afa4; }
}
`

// isRemoteURL reports whether a (possibly quoted) URL value points at a remote
// resource that should be blocked until the user opts in. Inline (cid:, data:)
// and relative references are left alone.
func isRemoteURL(v string) bool {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
	low := strings.ToLower(v)
	return strings.HasPrefix(low, "http://") ||
		strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "//")
}

// blockRemoteContent rewrites <img> tags (and inline style/background image
// references) so no remote asset loads automatically. It returns the rewritten
// HTML and whether anything was blocked.
func blockRemoteContent(html string) (string, bool) {
	blocked := false

	out := imgTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		// src=
		tag = srcAttrRe.ReplaceAllStringFunc(tag, func(attr string) string {
			val := srcAttrRe.FindStringSubmatch(attr)[1]
			if !isRemoteURL(val) {
				return attr
			}
			blocked = true
			return `src="` + blockedImgPlaceholder + `" data-blocked-src=` + ensureQuoted(val)
		})
		// srcset=
		tag = srcsetRe.ReplaceAllStringFunc(tag, func(attr string) string {
			val := srcsetRe.FindStringSubmatch(attr)[1]
			blocked = true
			return `data-blocked-srcset=` + val
		})
		return tag
	})

	// background="http..." attributes on any element.
	out = bgAttrRe.ReplaceAllStringFunc(out, func(attr string) string {
		val := bgAttrRe.FindStringSubmatch(attr)[1]
		if !isRemoteURL(val) {
			return attr
		}
		blocked = true
		return `data-blocked-background=` + ensureQuoted(val)
	})

	// Remote url(...) inside inline style="" attributes.
	out = styleAttrRe.ReplaceAllStringFunc(out, func(attr string) string {
		if cssURLRe.MatchString(attr) {
			blocked = true
			return cssURLRe.ReplaceAllString(attr, "url(about:blank#blocked")
		}
		return attr
	})

	return out, blocked
}

// ensureQuoted returns v wrapped in double quotes if it is not already quoted.
func ensureQuoted(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		return v
	}
	return `"` + v + `"`
}

// prepareEmailHTML wraps a raw HTML email body in a minimal document with our
// baseline stylesheet and a <base target="_blank"> (so links open in a new tab
// instead of trying — and failing — to navigate the sandboxed frame). It blocks
// remote images by default and reports whether any were blocked.
//
// The returned string is a plain string: the template assigns it to the iframe
// srcdoc attribute, where html/template attribute-escapes it correctly.
func prepareEmailHTML(raw string) (string, bool) {
	body, blocked := blockRemoteContent(raw)

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<base target="_blank"><style>`)
	b.WriteString(emailFrameCSS)
	b.WriteString(`</style></head><body>`)
	b.WriteString(body)
	b.WriteString(`</body></html>`)
	return b.String(), blocked
}
