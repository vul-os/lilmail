// handlers/jsonapi/snooze.go — POST/DELETE /v1/messages/:uid/snooze.
//
// Snooze hides a message until a due time. lilmail is a CLIENT and does not run
// the inbound delivery path, so it owns only the reversible IMAP half: it moves
// the message to the Snoozed folder (creating it if the account has none). The
// automatic RETURN of the message to the inbox at the due instant requires a
// delivery-side scheduler that lilmail does not host, so the response is honest:
// the message is moved (snoozed), but auto-return is reported as unavailable and
// the client is expected to surface / handle the due time itself.
//
// DELETE is the inverse intent (cancel a snooze); the caller moves the message
// back itself — this endpoint is a no-op acknowledgement kept for API symmetry.
package jsonapi

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// handleSnooze moves a message to the Snoozed folder. `until` is validated and
// echoed so the client can track the due time, but lilmail does not itself return
// the message to the inbox (no delivery-side scheduler) — autoReturn is false.
// POST /v1/messages/:uid/snooze  ?folder=  body {until}
func (h *Handler) handleSnooze(c *fiber.Ctx) error {
	uid := c.Params("uid")
	src := folderParam(c)

	var body struct {
		Until string `json:"until"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {until}")
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(body.Until))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "until must be an RFC3339 timestamp")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	snoozed, err := cl.DiscoverSnoozedFolder()
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not locate snoozed folder")
	}
	if strings.EqualFold(snoozed, src) {
		return fail(c, fiber.StatusBadRequest, "message is already snoozed")
	}
	if err := cl.MoveMessage(src, uid, snoozed); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not snooze message")
	}

	// 200 (not 204) with a body so the client can surface the caveat: the message
	// IS snoozed (moved), but lilmail does not auto-return it to the inbox.
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"snoozed":    true,
		"autoReturn": false,
		"until":      until.UTC().Format(time.RFC3339),
		"folder":     snoozed,
		"note":       "message moved to the Snoozed folder; automatic return to the inbox is handled by the client",
	})
}

// handleUnsnooze is a no-op acknowledgement kept for API symmetry: lilmail holds
// no due-time schedule to clear (the caller moves the message back itself).
// DELETE /v1/messages/:uid/snooze  ?folder=
func (h *Handler) handleUnsnooze(c *fiber.Ctx) error {
	return c.SendStatus(fiber.StatusNoContent)
}
