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
// Privacy / remote content
// -------------------------
// Remote (http/https/protocol-relative "//") assets are neutralised server-side
// before the HTML ever reaches the browser, so no tracking pixel or remote asset
// loads until the user explicitly clicks "Display images". The vectors that are
// neutralised are:
//   - <img>/<image> src (→ placeholder) and srcset
//   - <input type=image> src
//   - <video> poster
//   - media src on <video>/<audio>/<source>/<track>/<embed>/<iframe>
//   - <object> data
//   - <link> href (e.g. remote stylesheets)
//   - the background="" attribute on any element
//   - remote url(...) and @import in inline style="" attributes AND in
//     <style>...</style> blocks
//
// Blocked <img>/<image> src/srcset URLs (and the other tag attributes) are
// stashed in data-blocked-* attributes; the parent page restores <img> ones on
// demand (it can, because the frame is same-origin and script-free). Remote CSS
// url()/@import are rewritten to about:blank and are NOT restorable — they are
// only blocked. Inline data: URIs (embedded/contact-photo images) and cid:/
// relative references are left untouched, so nothing local is over-blocked.
package web

import (
	"regexp"
	"strings"
)

// transparent 1x1 GIF — used as the placeholder for blocked remote images so
// layout/dimensions are preserved without leaking a request.
const blockedImgPlaceholder = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

var (
	// tagRe matches a single start tag so blockRemoteContent can dispatch on the
	// tag name. Case-insensitive; capture group 1 is the (possibly upper-case)
	// tag name. Closing tags, comments and doctype are not letters after "<" so
	// they are skipped. Like the attribute regexes below it stops the tag at the
	// first ">", so an attribute value containing a literal ">" is not handled —
	// acceptable for the privacy pass (the XSS sanitiser is a separate stage).
	tagRe = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b[^>]*>`)

	srcAttrRe    = regexp.MustCompile(`(?is)\bsrc\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	srcsetRe     = regexp.MustCompile(`(?is)\bsrcset\s*=\s*("[^"]*"|'[^']*')`)
	posterAttrRe = regexp.MustCompile(`(?is)\bposter\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	hrefAttrRe   = regexp.MustCompile(`(?is)\bhref\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	dataAttrRe   = regexp.MustCompile(`(?is)\bdata\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	bgAttrRe     = regexp.MustCompile(`(?is)\bbackground\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	styleAttrRe  = regexp.MustCompile(`(?is)style\s*=\s*("[^"]*"|'[^']*')`)
	styleBlockRe = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	// inputImageRe reports whether an <input> tag is an image button (only those
	// fetch a remote resource via src).
	inputImageRe = regexp.MustCompile(`(?is)\btype\s*=\s*['"]?\s*image\b`)

	// Remote url(...) and @import (string form) inside CSS. Both consume the whole
	// reference — including the remote host — so nothing of the original URL is
	// left behind. @import url(...) is covered by cssURLRe; cssImportRe handles the
	// bare-string form @import "http://…".
	cssURLRe    = regexp.MustCompile(`(?is)url\(\s*['"]?\s*(?:https?:|//)[^)]*\)`)
	cssImportRe = regexp.MustCompile(`(?is)@import\s+['"]\s*(?:https?:|//)[^'"]*['"]`)
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

// blockRemoteContent neutralises every remote-fetching vector in an HTML mail
// body so no remote asset (tracking pixel, remote CSS, media, etc.) loads
// automatically. It rewrites: <img>/<image> src (→ placeholder + data-blocked-src)
// and srcset; <input type=image> src; <video> poster; src on
// <video>/<audio>/<source>/<track>/<embed>/<iframe>; <object> data; <link> href;
// the background="" attribute on any element; and remote url()/@import inside both
// inline style="" attributes and <style> blocks. "Remote" means http/https or a
// protocol-relative "//" URL; cid:, data: and relative references are left alone.
// It returns the rewritten HTML and whether anything was blocked (which drives the
// "Display images" banner).
func blockRemoteContent(html string) (string, bool) {
	blocked := false

	// 1. Remote url(...) / @import inside <style>...</style> blocks. Done first so
	//    the tag pass below only has to worry about start-tag attributes.
	html = styleBlockRe.ReplaceAllStringFunc(html, func(block string) string {
		out, ok := neutralizeCSS(block)
		if ok {
			blocked = true
		}
		return out
	})

	// 2. Dispatch on each start tag and neutralise its remote-fetching attributes.
	html = tagRe.ReplaceAllStringFunc(html, func(tag string) string {
		name := strings.ToLower(tagRe.FindStringSubmatch(tag)[1])
		out, ok := neutralizeTag(name, tag)
		if ok {
			blocked = true
		}
		return out
	})

	// 3. background="http..." attributes on any element.
	html = bgAttrRe.ReplaceAllStringFunc(html, func(attr string) string {
		val := bgAttrRe.FindStringSubmatch(attr)[1]
		if !isRemoteURL(val) {
			return attr
		}
		blocked = true
		return `data-blocked-background=` + ensureQuoted(val)
	})

	// 4. Remote url(...) / @import inside inline style="" attributes.
	html = styleAttrRe.ReplaceAllStringFunc(html, func(attr string) string {
		out, ok := neutralizeCSS(attr)
		if ok {
			blocked = true
		}
		return out
	})

	return html, blocked
}

// neutralizeTag rewrites the remote-fetching attributes of a single start tag,
// dispatching on the (lower-cased) tag name. It returns the rewritten tag and
// whether anything was blocked. Tags with no remote-fetching attribute (e.g. <a>,
// whose href is a user-followed link, not an auto-loaded asset) are returned
// unchanged.
func neutralizeTag(name, tag string) (string, bool) {
	switch name {
	// <image> is parsed as <img> by the HTML parser, so treat both identically.
	case "img", "image":
		tag, b1 := blockSrcPlaceholder(tag)
		tag, b2 := blockSrcset(tag)
		return tag, b1 || b2
	case "input":
		if !inputImageRe.MatchString(tag) {
			return tag, false
		}
		return blockSrcPlaceholder(tag)
	case "source":
		tag, b1 := stashAttr(tag, srcAttrRe, "data-blocked-src")
		tag, b2 := blockSrcset(tag)
		return tag, b1 || b2
	case "video":
		tag, b1 := stashAttr(tag, posterAttrRe, "data-blocked-poster")
		tag, b2 := stashAttr(tag, srcAttrRe, "data-blocked-src")
		return tag, b1 || b2
	case "audio", "track", "embed", "iframe":
		return stashAttr(tag, srcAttrRe, "data-blocked-src")
	case "link":
		return stashAttr(tag, hrefAttrRe, "data-blocked-href")
	case "object":
		return stashAttr(tag, dataAttrRe, "data-blocked-data")
	}
	return tag, false
}

// blockSrcPlaceholder swaps a remote src= for a transparent placeholder and
// stashes the original URL in data-blocked-src (which the parent page restores
// when the user clicks "Display images"). data:/cid:/relative src are left alone.
func blockSrcPlaceholder(tag string) (string, bool) {
	blocked := false
	tag = srcAttrRe.ReplaceAllStringFunc(tag, func(attr string) string {
		val := srcAttrRe.FindStringSubmatch(attr)[1]
		if !isRemoteURL(val) {
			return attr
		}
		blocked = true
		return `src="` + blockedImgPlaceholder + `" data-blocked-src=` + ensureQuoted(val)
	})
	return tag, blocked
}

// blockSrcset stashes a srcset= into data-blocked-srcset. srcset is a candidate
// list; if it is present at all it is stashed wholesale (matching the historical
// <img> behaviour) so no candidate is fetched.
func blockSrcset(tag string) (string, bool) {
	blocked := false
	tag = srcsetRe.ReplaceAllStringFunc(tag, func(attr string) string {
		val := srcsetRe.FindStringSubmatch(attr)[1]
		blocked = true
		return `data-blocked-srcset=` + val
	})
	return tag, blocked
}

// stashAttr renames a remote-URL-bearing attribute (matched by re) to dataName so
// the browser will not fetch it, preserving the original value for possible later
// restoration. Non-remote (data:/cid:/relative) values are left untouched.
func stashAttr(tag string, re *regexp.Regexp, dataName string) (string, bool) {
	blocked := false
	tag = re.ReplaceAllStringFunc(tag, func(attr string) string {
		val := re.FindStringSubmatch(attr)[1]
		if !isRemoteURL(val) {
			return attr
		}
		blocked = true
		return dataName + `=` + ensureQuoted(val)
	})
	return tag, blocked
}

// neutralizeCSS rewrites remote url(...) and @import references inside a chunk of
// CSS (a <style> block or an inline style="" attribute) to about:blank so nothing
// is fetched. It returns the rewritten CSS and whether anything was changed. These
// rewrites are destructive (not restorable), which is why remote CSS is only ever
// blocked, never surfaced by the "Display images" restore path.
func neutralizeCSS(css string) (string, bool) {
	changed := false
	if cssURLRe.MatchString(css) {
		changed = true
		css = cssURLRe.ReplaceAllString(css, "url(about:blank#blocked)")
	}
	if cssImportRe.MatchString(css) {
		changed = true
		css = cssImportRe.ReplaceAllString(css, `@import "about:blank#blocked"`)
	}
	return css, changed
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
