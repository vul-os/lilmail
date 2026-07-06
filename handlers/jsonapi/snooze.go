// handlers/jsonapi/snooze.go — POST/DELETE /v1/messages/:uid/snooze.
//
// Snooze hides a message until a due time, then returns it to the inbox. lilmail
// owns the reversible IMAP half (move the message to the Snoozed folder), while
// the authoritative DUE-TIME schedule + the un-snooze action live in vulos-mail
// (where delivery runs, so a message can actually re-appear in the inbox at the
// due instant). This handler:
//
//  1. reads the message's RFC822 Message-ID (the stable identifier shared with
//     vulos-mail's model, so no fragile IMAP-UID<->model-ID mapping is needed),
//  2. moves the message from its source folder to the Snoozed folder (IMAP MOVE,
//     creating the folder if the account has none),
//  3. registers the due-time with vulos-mail's broker-gated /internal/snooze
//     endpoint (URL derived from the brokered rule-store URL — the two live under
//     the same /internal base). If no such backend is brokered (session lilmail,
//     or a plain Gmail/IMAP account), the move still happens and the response
//     notes the auto-return is not wired for this backend (honest degrade).
//
// DELETE cancels a pending snooze (best-effort) — the caller is expected to move
// the message back itself; this only clears the scheduled auto-return.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// snoozeStoreURLFromRules derives vulos-mail's /internal/snooze base URL from the
// brokered rule-store URL (…/internal/mailrules → …/internal/snooze). Returns ""
// when no rule-store URL is brokered (the snooze schedule cannot be persisted).
func snoozeStoreURLFromRules(rulesURL string) string {
	rulesURL = strings.TrimRight(strings.TrimSpace(rulesURL), "/")
	if rulesURL == "" {
		return ""
	}
	if strings.HasSuffix(rulesURL, "/internal/mailrules") {
		return strings.TrimSuffix(rulesURL, "/mailrules") + "/snooze"
	}
	// Unknown shape: best-effort sibling under the same parent path.
	if i := strings.LastIndex(rulesURL, "/"); i > 0 {
		return rulesURL[:i] + "/snooze"
	}
	return ""
}

// postSnoozeSchedule registers (POST) or cancels (DELETE) a due-time with
// vulos-mail's /internal/snooze endpoint. Package var for the test seam.
var postSnoozeSchedule = func(ctx context.Context, storeURL, secret, method, account, messageID string, until time.Time) error {
	body := map[string]any{"account": account, "messageId": messageID}
	if method == http.MethodPost {
		body["until"] = until.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, storeURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vulos-Broker-Auth", secret)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &fiber.Error{Code: resp.StatusCode, Message: "snooze schedule rejected"}
	}
	return nil
}

// handleSnooze moves a message to the Snoozed folder and schedules its return to
// the inbox at `until`.
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

	// Read the Message-ID before the move (the UID changes across the move).
	msg, err := cl.FetchSingleMessage(src, uid)
	if err != nil {
		return fail(c, fiber.StatusNotFound, "message not found")
	}
	messageID := strings.TrimSpace(msg.MessageID)

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

	// Register the due-time with vulos-mail so the message returns to the inbox.
	// Requires both a brokered rule-store URL (to derive the snooze endpoint) and
	// a Message-ID to key on. Without either, the move is done but the auto-return
	// is not scheduled — report that honestly instead of pretending it will fire.
	scheduled := false
	if spec, ok := brokerSpecOf(c); ok && messageID != "" {
		if storeURL := snoozeStoreURLFromRules(spec.RulesURL); storeURL != "" {
			if err := postSnoozeSchedule(c.Context(), storeURL, h.brokerSecret, http.MethodPost, spec.Email, messageID, until); err == nil {
				scheduled = true
			}
		}
	}
	if !scheduled {
		// 200 (not 204) with a body so the client can surface the caveat if it
		// wants; the message IS snoozed (moved), just not auto-scheduled to return.
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"snoozed":    true,
			"autoReturn": false,
			"until":      until.UTC().Format(time.RFC3339),
			"folder":     snoozed,
			"note":       "message moved to the Snoozed folder; automatic return to the inbox is not available for this mailbox backend",
		})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleUnsnooze cancels a pending scheduled return for a message. It does not
// move the message (the caller does that); it only clears the schedule so the
// message will not later re-appear in the inbox on its own.
// DELETE /v1/messages/:uid/snooze  ?folder=
func (h *Handler) handleUnsnooze(c *fiber.Ctx) error {
	uid := c.Params("uid")
	src := folderParam(c)

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	msg, err := cl.FetchSingleMessage(src, uid)
	if err != nil {
		return fail(c, fiber.StatusNotFound, "message not found")
	}
	messageID := strings.TrimSpace(msg.MessageID)

	if spec, ok := brokerSpecOf(c); ok && messageID != "" {
		if storeURL := snoozeStoreURLFromRules(spec.RulesURL); storeURL != "" {
			_ = postSnoozeSchedule(c.Context(), storeURL, h.brokerSecret, http.MethodDelete, spec.Email, messageID, time.Time{})
		}
	}
	return c.SendStatus(fiber.StatusNoContent)
}
