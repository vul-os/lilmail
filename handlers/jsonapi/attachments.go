// handlers/jsonapi/attachments.go — JSON-API attachment download.
//
// GET /v1/messages/:uid/attachments/:partId streams a single MIME part. It is
// the machine-readable counterpart of the HTMX web download (handlers/web/
// email.go HandleAttachment) and works in BOTH auth modes:
//
//   - session mode: the MailClient comes from the caller's session.
//   - CP-brokered mode: the MailClient is built from the validated X-Vulos-Mail-*
//     headers, so a control-plane-proxied client can download attachments without
//     ever holding a lilmail session.
//
// As in the web path, the OPTIONAL object-storage cache (storage.ObjectStore-
// FromHeaders) is wired in: when the Vulos gateway has provisioned a bucket for
// the request, immutable attachment blobs are served from / populated into it so
// repeated downloads don't re-pull the full part from IMAP. Absent those headers
// it is a pure no-op and IMAP remains the source of truth.
package jsonapi

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"

	"lilmail/storage"

	"github.com/gofiber/fiber/v2"
)

// maxAttachmentBytes caps a single attachment download, matching the web path.
const maxAttachmentBytes = 25 * 1024 * 1024

// handleAttachment streams the attachment identified by the message UID, the
// MIME part path (partId), and the `folder` query param (default INBOX).
// GET /v1/messages/:uid/attachments/:partId  ?folder=
func (h *Handler) handleAttachment(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")
	partID := c.Params("partId")
	if dec, err := url.PathUnescape(partID); err == nil {
		partID = dec
	}
	if uid == "" || partID == "" {
		return fail(c, fiber.StatusBadRequest, "uid and partId are required")
	}

	// Optional supplementary read-through cache (no-op when the gateway hasn't
	// provisioned a bucket / the seam is gated off). Cache trouble must never
	// break a download, so failures only log and fall through to IMAP.
	objStore, useCache := storage.ObjectStoreFromHeaders(func(k string) string { return c.Get(k) })
	cacheKey := attachmentCacheKey(folder, uid, partID)
	if useCache {
		if obj, cerr := objStore.Get(c.UserContext(), cacheKey); cerr == nil {
			return streamAttachment(c, obj.Body, obj.Meta["filename"], obj.ContentType)
		} else if cerr != storage.ErrNotFound {
			log.Printf("jsonapi: attachment cache get %s: %v", cacheKey, cerr)
		}
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	content, filename, contentType, err := cl.FetchAttachment(folder, uid, partID)
	if err != nil {
		log.Printf("jsonapi: fetch attachment %s/%s/%s: %v", folder, uid, partID, err)
		return fail(c, fiber.StatusNotFound, "attachment not found")
	}
	if len(content) > maxAttachmentBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "attachment exceeds maximum allowed size")
	}

	// Best-effort cache population; never surfaced (download already succeeded).
	if useCache {
		if perr := objStore.Put(c.UserContext(), cacheKey, content, contentType, map[string]string{"filename": filename}); perr != nil {
			log.Printf("jsonapi: attachment cache put %s: %v", cacheKey, perr)
		}
	}

	return streamAttachment(c, content, filename, contentType)
}

// streamAttachment writes the body with the correct Content-Type and a download
// Content-Disposition. Both the content type and the filename originate from
// message MIME headers / a caller-supplied upload, so they are UNTRUSTED and are
// sanitized here against HTTP response-header injection (CR/LF/NUL) before being
// echoed into response headers. See sanitizeContentType / contentDisposition.
func streamAttachment(c *fiber.Ctx, body []byte, filename, contentType string) error {
	c.Set("Content-Type", sanitizeContentType(contentType))
	c.Set("Content-Disposition", contentDisposition(filename))
	return c.SendStream(bytes.NewReader(body), len(body))
}

// mimeTypeRe matches a conservative RFC 2045 "type/subtype" (optionally with
// parameters). A value that does not match — including anything carrying CR, LF
// or NUL — is rejected so it can never break out of the Content-Type header.
var mimeTypeRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+/[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+(\s*;[^\r\n\x00]*)?$`)

// sanitizeContentType returns a safe MIME type for the Content-Type header,
// falling back to application/octet-stream when the value is empty or does not
// look like a well-formed, injection-free media type.
func sanitizeContentType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" || !mimeTypeRe.MatchString(ct) {
		return "application/octet-stream"
	}
	return ct
}

// contentDisposition builds an injection-safe attachment Content-Disposition.
// Control characters (which include the CR/LF used for header splitting) are
// stripped from the filename; the ASCII form is quoted with quotes/backslashes
// escaped. When the (cleaned) name contains non-ASCII bytes an RFC 5987
// filename* form is appended so the original name still round-trips, while the
// quoted ASCII form remains as the legacy fallback.
func contentDisposition(filename string) string {
	cleaned := stripControl(filename)
	if cleaned == "" {
		return "attachment"
	}
	ascii, isASCII := asciiFold(cleaned)
	quoted := strings.ReplaceAll(ascii, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	if isASCII {
		return fmt.Sprintf(`attachment; filename="%s"`, quoted)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, quoted, rfc5987Escape(cleaned))
}

// stripControl removes C0 control characters and DEL (this covers CR, LF, NUL).
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// asciiFold replaces any non-ASCII rune with '_' and reports whether the input
// was already pure printable ASCII (so the caller can skip the filename* form).
func asciiFold(s string) (string, bool) {
	isASCII := true
	folded := strings.Map(func(r rune) rune {
		if r > 0x7f {
			isASCII = false
			return '_'
		}
		return r
	}, s)
	return folded, isASCII
}

// rfc5987Escape percent-encodes a UTF-8 string per RFC 5987 ext-value rules
// (attr-char stays literal; everything else becomes %HH). The input is already
// control-stripped, so the result can never contain CR/LF.
func rfc5987Escape(s string) string {
	const upperhex = "0123456789ABCDEF"
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(attrChars, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// attachmentCacheKey builds a stable, collision-free object key for the cache.
// It mirrors the web path's "attachments/" namespace; the storage seam further
// namespaces everything under the gateway prefix + "mail/".
func attachmentCacheKey(folder, uid, partID string) string {
	return "attachments/" + url.PathEscape(folder) + "/" + url.PathEscape(uid) + "/" + url.PathEscape(partID)
}
