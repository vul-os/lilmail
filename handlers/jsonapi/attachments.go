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
// Content-Disposition.
func streamAttachment(c *fiber.Ctx, body []byte, filename, contentType string) error {
	if contentType != "" {
		c.Set("Content-Type", contentType)
	}
	if filename != "" {
		c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	} else {
		c.Set("Content-Disposition", "attachment")
	}
	return c.SendStream(bytes.NewReader(body), len(body))
}

// attachmentCacheKey builds a stable, collision-free object key for the cache.
// It mirrors the web path's "attachments/" namespace; the storage seam further
// namespaces everything under the gateway prefix + "mail/".
func attachmentCacheKey(folder, uid, partID string) string {
	return "attachments/" + url.PathEscape(folder) + "/" + url.PathEscape(uid) + "/" + url.PathEscape(partID)
}
