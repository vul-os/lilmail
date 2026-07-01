// handlers/jsonapi/calendar.go — JSON calendar endpoints over CalDAV.
//
// These reuse the same CalDAV client (api.CalDAVClient) and models.Calendar*
// types as the HTMX calendar UI (handlers/web/calendar.go); only the transport
// is JSON. They are registered only when [caldav] enabled = true.
//
// Times travel as RFC 3339 strings (e.g. 2026-06-26T10:00:00Z). The start/end
// range defaults to the current month when omitted.
package jsonapi

import (
	"context"
	"time"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// parseRange parses ?start= and ?end= as RFC 3339, defaulting to the current
// calendar month when either is absent or unparseable.
func parseRange(c *fiber.Ctx) (time.Time, time.Time) {
	now := time.Now()
	defStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	defEnd := defStart.AddDate(0, 1, 0)

	start := defStart
	if s := c.Query("start"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			start = t
		}
	}
	end := defEnd
	if e := c.Query("end"); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			end = t
		}
	}
	return start, end
}

// calDAVClient returns a CalDAV client for the request. For CP-brokered requests
// it is built directly from the X-Vulos-Mail-Caldav-Url + bearer-token headers
// (never the session); otherwise it comes from the session via the AuthHandler.
// The returned bool is false when the request is brokered but the account has no
// CalDAV URL — the caller must respond "not available for this account" WITHOUT
// touching the session.
func (h *Handler) calDAVClient(c *fiber.Ctx) (calDAVClient, bool, error) {
	if spec, ok := brokerSpecOf(c); ok {
		if spec.CalDAVURL == "" {
			return nil, false, nil
		}
		cl, err := brokerDialCalDAV(spec)
		return cl, true, err
	}
	cl, err := h.auth.CalDAVClient(c)
	return cl, true, err
}

// handleCalendarEvents lists events in a time range.
// GET /v1/calendar/events?start=&end= → { events: CalendarEvent[] }
func (h *Handler) handleCalendarEvents(c *fiber.Ctx) error {
	start, end := parseRange(c)

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		// Brokered account without a CalDAV URL: empty result, no session.
		return c.JSON(fiber.Map{"events": []models.CalendarEvent{}})
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	events, err := cl.ListEvents(context.Background(), start, end)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not list events")
	}
	if events == nil {
		events = []models.CalendarEvent{}
	}
	return c.JSON(fiber.Map{"events": events})
}

// eventBody is the JSON payload for creating/updating an event. Times are RFC 3339.
type eventBody struct {
	UID         string `json:"uid"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Start       string `json:"start"`
	End         string `json:"end"`
	AllDay      bool   `json:"allDay"`
	Recurrence  string `json:"recurrence"`
	// Path is the CalDAV object path (from listEvents). When present on an update
	// it targets the exact object so an edit never forks a duplicate.
	Path string `json:"path"`
}

// handleCreateEvent creates a calendar event.
// POST /v1/calendar/events  body {summary, start, end, description?, location?, allDay?}
func (h *Handler) handleCreateEvent(c *fiber.Ctx) error {
	var body eventBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if body.Summary == "" {
		return fail(c, fiber.StatusBadRequest, "summary is required")
	}

	start, err := time.Parse(time.RFC3339, body.Start)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "start must be an RFC 3339 timestamp")
	}
	end, err := time.Parse(time.RFC3339, body.End)
	if err != nil || !end.After(start) {
		end = start.Add(time.Hour)
	}

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "calendar not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	ev := models.CalendarEvent{
		UID:         body.UID,
		Summary:     body.Summary,
		Description: body.Description,
		Location:    body.Location,
		Start:       start,
		End:         end,
		AllDay:      body.AllDay,
		Recurrence:  body.Recurrence,
		Path:        body.Path,
	}
	if err := cl.CreateEvent(context.Background(), ev); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not create event")
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"created": true})
}

// handleUpdateEvent overwrites an existing calendar event (idempotent PUT).
// PUT /v1/calendar/events/:uid  body {summary, start, end, description?, location?, allDay?, recurrence?, path?}
func (h *Handler) handleUpdateEvent(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "event uid required")
	}

	var body eventBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if body.Summary == "" {
		return fail(c, fiber.StatusBadRequest, "summary is required")
	}

	start, err := time.Parse(time.RFC3339, body.Start)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "start must be an RFC 3339 timestamp")
	}
	end, err := time.Parse(time.RFC3339, body.End)
	if err != nil || !end.After(start) {
		end = start.Add(time.Hour)
	}

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "calendar not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	ev := models.CalendarEvent{
		UID:         uid,
		Summary:     body.Summary,
		Description: body.Description,
		Location:    body.Location,
		Start:       start,
		End:         end,
		AllDay:      body.AllDay,
		Recurrence:  body.Recurrence,
		Path:        body.Path,
	}
	if err := cl.UpdateEvent(context.Background(), ev); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not update event")
	}
	return c.JSON(fiber.Map{"updated": true})
}

// handleDeleteEvent removes a calendar event by UID.
// DELETE /v1/calendar/events/:uid → 204
func (h *Handler) handleDeleteEvent(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "event uid required")
	}

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "calendar not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	if err := cl.DeleteEvent(context.Background(), uid); err != nil {
		return fail(c, fiber.StatusNotFound, "event not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleFreeBusy returns busy intervals in a time range.
// GET /v1/calendar/freebusy?start=&end= → { busy: FreeBusySlot[] }
func (h *Handler) handleFreeBusy(c *fiber.Ctx) error {
	start, end := parseRange(c)

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return c.JSON(fiber.Map{"busy": []models.FreeBusySlot{}})
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	slots, err := cl.FreeBusy(context.Background(), start, end)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not compute free/busy")
	}
	if slots == nil {
		slots = []models.FreeBusySlot{}
	}
	return c.JSON(fiber.Map{"busy": slots})
}
