// handlers/jsonapi/contact_photo.go — the dedicated contact-photo endpoints.
//
// The raster-only / size-capped / content-sniffed validation itself lives in the
// api package (api.ValidatePhotoBytes / api.NormalizePhotoURI) so the JSON write
// path, this multipart upload endpoint and the vCard round-trip all share exactly
// ONE gate. See handlers/api/contact_photo.go for the security rationale.
//
//	POST   /v1/contacts/:uid/photo   multipart {file}  → { contact }
//	DELETE /v1/contacts/:uid/photo                      → { contact }
//
// Isolation is inherited from the contact CRUD seam: the upload re-reads and
// re-writes through h.listAllContacts / h.putContact, which target the CALLER'S
// OWN address book only — there is no account parameter, so a caller can only ever
// set a photo on a card in their own book.
package jsonapi

import (
	"io"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// normalizePhoto is the sanitize-path entry point; it defers to the shared api
// gate so every JSON create/update/import gets the same raster/size validation.
func normalizePhoto(s string) string { return api.NormalizePhotoURI(s) }

// registerContactPhoto mounts the photo routes. Registered before the /:uid PUT so
// the literal "photo" sub-path is unambiguous.
func (h *Handler) registerContactPhoto(g fiber.Router) {
	g.Post("/contacts/:uid/photo", h.handleUploadPhoto)
	g.Delete("/contacts/:uid/photo", h.handleDeletePhoto)
}

// handleUploadPhoto accepts a multipart image upload and stores it on the contact.
func (h *Handler) handleUploadPhoto(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "contact uid required")
	}
	if _, _, ok := h.cardDAVAvailable(c); !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "a file field is required")
	}
	if fileHeader.Size > api.MaxPhotoBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "image too large")
	}
	f, err := fileHeader.Open()
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "could not read upload")
	}
	defer f.Close()
	// Read at most MaxPhotoBytes+1 so an over-cap file is detected, not truncated.
	raw, err := io.ReadAll(io.LimitReader(f, api.MaxPhotoBytes+1))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "could not read upload")
	}
	uri, ok := api.ValidatePhotoBytes(raw)
	if !ok {
		return fail(c, fiber.StatusUnsupportedMediaType, "photo must be a PNG, JPEG, GIF or WebP image within size limits")
	}

	// Locate the contact in the caller's own book and attach the photo.
	contacts, err := h.listAllContacts(c, "", 0)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not read contact")
	}
	for _, ct := range contacts {
		if ct.UID != uid || isPlaceholderGroupCard(ct) {
			continue
		}
		ct.Photo = uri
		saved, err := h.putContact(c, sanitizeContact(ct))
		if err != nil {
			return fail(c, fiber.StatusBadGateway, "could not save photo")
		}
		return c.JSON(fiber.Map{"contact": saved})
	}
	return fail(c, fiber.StatusNotFound, "contact not found")
}

// handleDeletePhoto removes a contact's photo.
func (h *Handler) handleDeletePhoto(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "contact uid required")
	}
	if _, _, ok := h.cardDAVAvailable(c); !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}
	contacts, err := h.listAllContacts(c, "", 0)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not read contact")
	}
	for _, ct := range contacts {
		if ct.UID != uid || isPlaceholderGroupCard(ct) {
			continue
		}
		ct.Photo = ""
		saved, err := h.putContact(c, sanitizeContact(ct))
		if err != nil {
			return fail(c, fiber.StatusBadGateway, "could not remove photo")
		}
		return c.JSON(fiber.Map{"contact": saved})
	}
	return fail(c, fiber.StatusNotFound, "contact not found")
}
