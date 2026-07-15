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
	"fmt"
	"log"
	"strings"
	"time"

	"lilmail/handlers/api"
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
	// Timezone is the IANA zone id for a timed event; empty means UTC.
	Timezone string `json:"timezone"`
	// Reminders are the VALARM alarms (popup/email + offset-before).
	Reminders []models.Reminder `json:"reminders"`
	// ExDates are excluded occurrence starts (RFC3339) — deleted single
	// instances of a recurring series.
	ExDates []string `json:"exdates"`
	// RecurrenceID (RFC3339), when set, makes this a single-instance override of
	// a recurring series.
	RecurrenceID string `json:"recurrenceId"`
	// Path is the CalDAV object path (from listEvents). When present on an update
	// it targets the exact object so an edit never forks a duplicate.
	Path string `json:"path"`
	// Attendees, when non-empty, makes this a meeting: the event is stored with
	// ORGANIZER/ATTENDEE properties and a METHOD:REQUEST iMIP invite is mailed to
	// each attendee. Attendees always come from the user's OWN event editor — they
	// are never harvested from received mail — which keeps the send path from
	// becoming a forwarding/spam vector. Count is capped at api.MaxAttendees.
	Attendees []models.Attendee `json:"attendees"`
	// Sequence is bumped by the client on a reschedule so invitees can tell an
	// update from a duplicate; defaults to 0 for a fresh event.
	Sequence int `json:"sequence"`
}

// toEvent maps an eventBody + parsed times into a models.CalendarEvent, filling
// the organizer with the sending identity when the event has attendees.
func (b eventBody) toEvent(uid, organizer string, start, end time.Time) models.CalendarEvent {
	ev := models.CalendarEvent{
		UID:         uid,
		Summary:     b.Summary,
		Description: b.Description,
		Location:    b.Location,
		Start:       start,
		End:         end,
		AllDay:      b.AllDay,
		Recurrence:  b.Recurrence,
		Timezone:    b.Timezone,
		Reminders:   b.Reminders,
		Path:        b.Path,
		Attendees:   b.Attendees,
		Sequence:    b.Sequence,
	}
	for _, s := range b.ExDates {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			ev.ExDates = append(ev.ExDates, t)
		}
	}
	if b.RecurrenceID != "" {
		if t, err := time.Parse(time.RFC3339, b.RecurrenceID); err == nil {
			ev.RecurrenceID = &t
		}
	}
	if len(b.Attendees) > 0 {
		ev.Organizer = organizer
	}
	return ev
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

	if len(body.Attendees) > api.MaxAttendees {
		return fail(c, fiber.StatusBadRequest, "too many attendees")
	}

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "calendar not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	uid := body.UID
	if uid == "" {
		uid = fmt.Sprintf("lilmail-%d@lilmail.local", start.UnixNano())
	}
	organizer := h.fromEmail(c)
	ev := body.toEvent(uid, organizer, start, end)
	if err := cl.CreateEvent(context.Background(), ev); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not create event")
	}

	// Meeting → mail a METHOD:REQUEST iMIP invite to each attendee (best effort;
	// the event is already stored, so a mail failure is reported but not fatal).
	invited, warn := h.sendInvites(c, ev, "REQUEST")
	resp := fiber.Map{"created": true, "uid": uid, "invited": invited}
	if warn != "" {
		resp["warning"] = warn
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
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

	if len(body.Attendees) > api.MaxAttendees {
		return fail(c, fiber.StatusBadRequest, "too many attendees")
	}

	cl, ok, err := h.calDAVClient(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "calendar not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "calendar not available")
	}

	organizer := h.fromEmail(c)
	ev := body.toEvent(uid, organizer, start, end)
	if err := cl.UpdateEvent(context.Background(), ev); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not update event")
	}

	// An organizer update to a meeting re-sends METHOD:REQUEST so invitees learn
	// of the reschedule (the bumped SEQUENCE distinguishes it from a duplicate).
	invited, warn := h.sendInvites(c, ev, "REQUEST")
	resp := fiber.Map{"updated": true, "invited": invited}
	if warn != "" {
		resp["warning"] = warn
	}
	return c.JSON(resp)
}

// sendInvites mails a METHOD:REQUEST (or CANCEL) iMIP invite to each attendee of
// ev. It returns the number of attendees mailed and a non-empty warning string
// when some (or all) sends failed. It is a no-op returning (0,"") when the event
// has no attendees or no organizer (i.e. a personal, non-meeting event), so a
// plain event create never generates mail.
func (h *Handler) sendInvites(c *fiber.Ctx, ev models.CalendarEvent, method string) (int, string) {
	if len(ev.Attendees) == 0 || ev.Organizer == "" {
		return 0, ""
	}
	orgName := api.GetUsernameFromEmail(ev.Organizer)

	ics, err := api.BuildRequestICS(api.InviteParams{
		Method:      method,
		UID:         ev.UID,
		Sequence:    ev.Sequence,
		Organizer:   ev.Organizer,
		OrgName:     orgName,
		Summary:     ev.Summary,
		Description: ev.Description,
		Location:    ev.Location,
		Start:       ev.Start,
		End:         ev.End,
		AllDay:      ev.AllDay,
		Recurrence:  ev.Recurrence,
		Attendees:   ev.Attendees,
	})
	if err != nil {
		log.Printf("jsonapi: build invite: %v", err)
		return 0, "invite not sent: " + err.Error()
	}

	sender, err := h.smtpClient(c)
	if err != nil {
		return 0, "invite not sent: mail server unavailable"
	}

	verb := "Invitation"
	if strings.EqualFold(method, "CANCEL") {
		verb = "Cancelled"
	}
	subject := verb + ": " + ev.Summary
	plain := inviteSummaryText(ev, method)

	sent := 0
	var failed []string
	for _, a := range ev.Attendees {
		addr := strings.TrimSpace(a.Email)
		if addr == "" {
			continue
		}
		raw, err := api.BuildIMIPMessage(ev.Organizer, orgName, addr, subject, plain, ics, method)
		if err != nil {
			failed = append(failed, addr)
			continue
		}
		if err := sender.SendRawMessage([]string{addr}, raw); err != nil {
			log.Printf("jsonapi: send invite to %s: %v", addr, err)
			failed = append(failed, addr)
			continue
		}
		sent++
	}
	warn := ""
	if len(failed) > 0 {
		warn = fmt.Sprintf("invite delivery failed for %d recipient(s)", len(failed))
	}
	return sent, warn
}

// inviteSummaryText renders the human-readable text/plain alternative for an
// iMIP invite so recipients on clients without calendar rendering still see the
// essentials.
func inviteSummaryText(ev models.CalendarEvent, method string) string {
	var b strings.Builder
	if strings.EqualFold(method, "CANCEL") {
		b.WriteString("This event has been cancelled.\r\n\r\n")
	} else {
		b.WriteString("You have been invited to an event.\r\n\r\n")
	}
	b.WriteString("Event: " + ev.Summary + "\r\n")
	when := ev.Start.Format("Mon, 02 Jan 2006 15:04 MST")
	if ev.AllDay {
		when = ev.Start.Format("Mon, 02 Jan 2006") + " (all day)"
	}
	b.WriteString("When: " + when + "\r\n")
	if ev.Location != "" {
		b.WriteString("Where: " + ev.Location + "\r\n")
	}
	if ev.Organizer != "" {
		b.WriteString("Organizer: " + ev.Organizer + "\r\n")
	}
	if ev.Description != "" {
		b.WriteString("\r\n" + ev.Description + "\r\n")
	}
	return b.String()
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

// rsvpBody is the payload for POST /v1/calendar/rsvp. It carries the details a
// client extracted from a received invite (message.invite) plus the chosen
// response, so the server can (1) mail a METHOD:REPLY to the organizer and
// (2) reflect the event in the responder's own calendar.
type rsvpBody struct {
	UID       string `json:"uid"`
	Organizer string `json:"organizer"`
	Response  string `json:"response"` // "accept" | "tentative" | "decline"
	// Optional event details echoed back so the accepted/tentative event can be
	// materialised in the responder's calendar. When absent, only the REPLY mail
	// is sent (still a valid RSVP; the calendar copy is skipped).
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Start       string `json:"start"`
	End         string `json:"end"`
	AllDay      bool   `json:"allDay"`
	Recurrence  string `json:"recurrence"`
	Sequence    int    `json:"sequence"`
}

// handleCalendarRSVP responds to an inbound meeting invitation.
// POST /v1/calendar/rsvp  body {uid, organizer, response, ...event}
//
//   - Sends a METHOD:REPLY iMIP message to the organizer carrying the attendee's
//     new PARTSTAT (RFC 5546/6047).
//   - On accept/tentative, adds/updates the event in the attendee's own calendar
//     with their PARTSTAT recorded, so it shows up alongside their other events.
//     On decline, any existing copy is removed.
func (h *Handler) handleCalendarRSVP(c *fiber.Ctx) error {
	var body rsvpBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	if strings.TrimSpace(body.UID) == "" {
		return fail(c, fiber.StatusBadRequest, "uid is required")
	}
	partstat := map[string]string{
		"accept":    "ACCEPTED",
		"accepted":  "ACCEPTED",
		"tentative": "TENTATIVE",
		"decline":   "DECLINED",
		"declined":  "DECLINED",
	}[strings.ToLower(strings.TrimSpace(body.Response))]
	if partstat == "" {
		return fail(c, fiber.StatusBadRequest, "response must be accept, tentative or decline")
	}

	attendee := h.fromEmail(c)
	if attendee == "" {
		return fail(c, fiber.StatusUnauthorized, "no sender identity")
	}

	// 1) Build + send the METHOD:REPLY to the organizer.
	ics, err := api.BuildReplyICS(api.ReplyParams{
		UID:          body.UID,
		Sequence:     body.Sequence,
		Organizer:    body.Organizer,
		Attendee:     attendee,
		AttendeeName: api.GetUsernameFromEmail(attendee),
		PartStat:     partstat,
		Summary:      body.Summary,
	})
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}

	organizer := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(body.Organizer), "mailto:"), "MAILTO:")
	replySent := false
	if organizer != "" {
		sender, err := h.smtpClient(c)
		if err != nil {
			return fail(c, fiber.StatusBadGateway, "failed to connect to mail server")
		}
		verb := map[string]string{"ACCEPTED": "Accepted", "TENTATIVE": "Tentative", "DECLINED": "Declined"}[partstat]
		subj := verb + ": " + body.Summary
		plain := fmt.Sprintf("%s has %s the invitation to \"%s\".\r\n", attendee, strings.ToLower(verb), body.Summary)
		raw, err := api.BuildIMIPMessage(attendee, api.GetUsernameFromEmail(attendee), organizer, subj, plain, ics, "REPLY")
		if err != nil {
			return fail(c, fiber.StatusInternalServerError, "failed to build reply")
		}
		if err := sender.SendRawMessage([]string{organizer}, raw); err != nil {
			log.Printf("jsonapi: rsvp reply send: %v", err)
			return fail(c, fiber.StatusBadGateway, "failed to send RSVP")
		}
		replySent = true
	}

	// 2) Reflect in the responder's own calendar (best effort; a CalDAV failure
	// does not undo the already-sent reply).
	calendarUpdated := h.reflectRSVP(c, body, attendee, partstat)

	return c.JSON(fiber.Map{
		"ok":              true,
		"partStat":        partstat,
		"replySent":       replySent,
		"calendarUpdated": calendarUpdated,
	})
}

// reflectRSVP materialises (accept/tentative) or removes (decline) the invited
// event in the responder's own calendar, recording their PARTSTAT. Returns false
// on any failure or when no CalDAV client / start time is available.
func (h *Handler) reflectRSVP(c *fiber.Ctx, body rsvpBody, attendee, partstat string) bool {
	cl, ok, err := h.calDAVClient(c)
	if !ok || err != nil {
		return false
	}

	if partstat == "DECLINED" {
		// Remove any prior copy; ignore "not found".
		_ = cl.DeleteEvent(context.Background(), body.UID)
		return true
	}

	start, err := time.Parse(time.RFC3339, body.Start)
	if err != nil {
		return false // no usable time to place the event
	}
	end, err := time.Parse(time.RFC3339, body.End)
	if err != nil || !end.After(start) {
		end = start.Add(time.Hour)
	}

	ev := models.CalendarEvent{
		UID:         body.UID,
		Summary:     body.Summary,
		Description: body.Description,
		Location:    body.Location,
		Organizer:   body.Organizer,
		Start:       start,
		End:         end,
		AllDay:      body.AllDay,
		Recurrence:  body.Recurrence,
		Sequence:    body.Sequence,
		Attendees:   []models.Attendee{{Email: attendee, PartStat: partstat}},
	}
	if err := cl.UpdateEvent(context.Background(), ev); err != nil {
		log.Printf("jsonapi: rsvp calendar update: %v", err)
		return false
	}
	return true
}
