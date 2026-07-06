// handlers/jsonapi/folders.go — /v1 folder (label) create/delete + report-spam.
//
// Labels in the mail-ui are derived from IMAP folders, so creating/deleting a
// label is creating/deleting a mailbox. These handlers back that surface. System
// folders (INBOX/Sent/Spam/Trash/Drafts/Archive/Snoozed) are protected: they can
// never be deleted, mirroring Gmail's behaviour and preserving the wave-17 safety
// posture (no destroying built-in mailboxes).
//
// handleReportSpam is the report-spam action: it moves a message to the Junk/Spam
// folder via the same IMAP MOVE the archive path uses. There is no spam-training
// signal endpoint on this backend (the anti-abuse chain trains on inbound
// delivery, not on client feedback), so this is honestly a move — the caller
// pairs it with an undo toast just like archive.
package jsonapi

import (
	"strings"

	"github.com/gofiber/fiber/v2"
)

// protectedFolders are the system mailboxes that must never be deleted. The
// match is case-insensitive and also covers the localised/variant names the
// discovery helpers recognise (Junk↔Spam, Deleted↔Trash, Bin).
var protectedFolders = map[string]bool{
	"inbox": true, "sent": true, "sent items": true, "drafts": true,
	"spam": true, "junk": true, "junk email": true, "bulk mail": true,
	"trash": true, "deleted": true, "deleted items": true, "bin": true,
	"archive": true, "all mail": true, "snoozed": true, "starred": true, "outbox": true,
}

// isProtectedFolder reports whether name is a system mailbox that must not be
// deleted. It checks the leaf segment too, so "INBOX/Sent" style hierarchies are
// judged by their final component.
func isProtectedFolder(name string) bool {
	lc := strings.ToLower(strings.TrimSpace(name))
	if protectedFolders[lc] {
		return true
	}
	if i := strings.LastIndexAny(lc, "/."); i >= 0 {
		if protectedFolders[strings.TrimSpace(lc[i+1:])] {
			return true
		}
	}
	return false
}

// registerFolders mounts the folder-management + report-spam routes. Always
// available (session or brokered) — they are plain IMAP operations.
func (h *Handler) registerFolders(g fiber.Router) {
	g.Post("/folders", h.handleCreateFolder)          // body {name}
	g.Delete("/folders", h.handleDeleteFolder)        // body {name} or ?folder=
	g.Post("/messages/:uid/spam", h.handleReportSpam) // ?folder=
}

// handleCreateFolder creates a new IMAP mailbox (label).
// POST /v1/folders  body {name}
func (h *Handler) handleCreateFolder(c *fiber.Ctx) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {name}")
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "folder name is required")
	}
	// Disallow the IMAP hierarchy/wildcard control characters and the reserved
	// system names as new-label targets (creating "INBOX" etc. is nonsensical).
	if strings.ContainsAny(name, "\r\n\t*%\"") {
		return fail(c, fiber.StatusBadRequest, "folder name contains invalid characters")
	}
	if isProtectedFolder(name) {
		return fail(c, fiber.StatusConflict, "a system folder with that name already exists")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	if err := cl.CreateMailbox(name); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not create folder")
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"folder": name})
}

// handleDeleteFolder deletes a user IMAP mailbox (label). System folders are
// refused. The target comes from the JSON body {name} or the ?folder= query.
// DELETE /v1/folders  body {name}  |  ?folder=
func (h *Handler) handleDeleteFolder(c *fiber.Ctx) error {
	var body struct {
		Name string `json:"name"`
	}
	_ = c.BodyParser(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(c.Query("folder"))
	}
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "folder name is required")
	}
	if isProtectedFolder(name) {
		return fail(c, fiber.StatusForbidden, "system folders cannot be deleted")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	if err := cl.DeleteMailbox(name); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not delete folder")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleReportSpam moves a message to the Junk/Spam folder. There is no separate
// training-signal endpoint on this backend — the move IS the report.
// POST /v1/messages/:uid/spam  ?folder=
func (h *Handler) handleReportSpam(c *fiber.Ctx) error {
	uid := c.Params("uid")
	src := folderParam(c)

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	junk, err := cl.DiscoverJunkFolder()
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not locate the spam folder")
	}
	if strings.EqualFold(junk, src) {
		return fail(c, fiber.StatusBadRequest, "message is already in the spam folder")
	}
	if err := cl.MoveMessage(src, uid, junk); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not report spam")
	}
	return c.JSON(fiber.Map{"folder": junk})
}
