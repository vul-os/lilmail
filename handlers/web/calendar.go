// handlers/web/calendar.go — CalDAV calendar route handlers for LilMail.
//
// All routes in this file are registered only when config.CalDAV.Enabled is
// true.  They share the same session-authenticated protected group used by the
// email handlers.
package web

import (
	"context"
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/models"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// CalendarHandler handles CalDAV calendar routes.
type CalendarHandler struct {
	store  *session.Store
	config *config.Config
	auth   *AuthHandler
}

// NewCalendarHandler creates a new CalendarHandler.
func NewCalendarHandler(store *session.Store, cfg *config.Config, auth *AuthHandler) *CalendarHandler {
	return &CalendarHandler{store: store, config: cfg, auth: auth}
}

// caldavClient returns a CalDAVClient authenticated for the current session.
// It delegates to AuthHandler.CalDAVClient so the HTMX calendar routes and the
// JSON API (/v1/calendar) share one CalDAV client-construction path.
func (h *CalendarHandler) caldavClient(c *fiber.Ctx) (*api.CalDAVClient, error) {
	return h.auth.CalDAVClient(c)
}

// monthBounds returns the first moment of a given month and the first moment
// of the following month (exclusive upper bound).
func monthBounds(year int, month time.Month) (start, end time.Time) {
	start = time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	end = start.AddDate(0, 1, 0)
	return
}

// HandleCalendarMonth renders the month-view calendar page.
// Query params: ?year=YYYY&month=M (both optional; defaults to current month).
func (h *CalendarHandler) HandleCalendarMonth(c *fiber.Ctx) error {
	now := time.Now()
	year := now.Year()
	month := now.Month()

	if y := c.Query("year"); y != "" {
		if n, err := strconv.Atoi(y); err == nil {
			year = n
		}
	}
	if m := c.Query("month"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n >= 1 && n <= 12 {
			month = time.Month(n)
		}
	}

	start, end := monthBounds(year, month)

	var events []models.CalendarEvent
	client, err := h.caldavClient(c)
	if err != nil {
		log.Printf("calendar: client error: %v", err)
		// Render an empty calendar rather than a hard error
	} else {
		ctx := context.Background()
		events, err = client.ListEvents(ctx, start, end)
		if err != nil {
			log.Printf("calendar: list events error: %v", err)
			events = nil
		}
	}

	// Build a 6-week grid (42 days) anchored to the first weekday of the month,
	// then populate each cell with its events.
	grid := buildMonthGrid(year, month, events)

	// Prev / Next month links
	prevYear, prevMonth := year, month-1
	if prevMonth < time.January {
		prevMonth = time.December
		prevYear--
	}
	nextYear, nextMonth := year, month+1
	if nextMonth > time.December {
		nextMonth = time.January
		nextYear++
	}

	return c.Render("calendar", fiber.Map{
		"Title":     fmt.Sprintf("%s %d", month.String(), year),
		"Year":      year,
		"Month":     int(month),
		"MonthName": month.String(),
		"Grid":      grid,
		"Today":     now,
		"PrevYear":  prevYear,
		"PrevMonth": int(prevMonth),
		"NextYear":  nextYear,
		"NextMonth": int(nextMonth),
	})
}

// HandleCalendarWeek renders the week-view calendar page.
// Query param: ?date=YYYY-MM-DD (optional; defaults to current week's Monday).
func (h *CalendarHandler) HandleCalendarWeek(c *fiber.Ctx) error {
	anchor := time.Now()
	if d := c.Query("date"); d != "" {
		if t, err := time.ParseInLocation("2006-01-02", d, time.Local); err == nil {
			anchor = t
		}
	}

	// Find the Monday of the week containing anchor.
	weekday := anchor.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	monday := anchor.AddDate(0, 0, -int(weekday-time.Monday))
	monday = time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.Local)
	sunday := monday.AddDate(0, 0, 7)

	var events []models.CalendarEvent
	client, err := h.caldavClient(c)
	if err != nil {
		log.Printf("calendar: client error: %v", err)
	} else {
		ctx := context.Background()
		events, err = client.ListEvents(ctx, monday, sunday)
		if err != nil {
			log.Printf("calendar: list events error: %v", err)
			events = nil
		}
	}

	// Build 7-slot day slice.
	days := make([]WeekDay, 7)
	for i := 0; i < 7; i++ {
		d := monday.AddDate(0, 0, i)
		days[i] = WeekDay{Date: d, Events: eventsOnDay(events, d)}
	}

	prevMonday := monday.AddDate(0, 0, -7)
	nextMonday := monday.AddDate(0, 0, 7)

	return c.Render("calendar-week", fiber.Map{
		"Title":    fmt.Sprintf("Week of %s", monday.Format("Jan 2, 2006")),
		"Monday":   monday,
		"Days":     days,
		"PrevWeek": prevMonday.Format("2006-01-02"),
		"NextWeek": nextMonday.Format("2006-01-02"),
		"Today":    time.Now(),
	})
}

// HandleEventDetail renders an event detail partial (HTMX target).
func (h *CalendarHandler) HandleEventDetail(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return c.Status(400).SendString("event UID required")
	}

	// We can't query a single event by UID without a REPORT; for now, search
	// within the current month and return the first match.  Full single-object
	// retrieval via GetCalendarObject requires the server path, which is opaque.
	now := time.Now()
	start := time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(2, 0, 0)

	client, err := h.caldavClient(c)
	if err != nil {
		return c.Status(500).SendString("calendar not available")
	}

	ctx := context.Background()
	events, err := client.ListEvents(ctx, start, end)
	if err != nil {
		return c.Status(500).SendString("failed to list events")
	}

	for _, ev := range events {
		if ev.UID == uid {
			return c.Render("partials/calendar-event", fiber.Map{
				"Event": ev,
			}, "") // no layout — HTMX partial
		}
	}

	return c.Status(404).SendString("event not found")
}

// HandleCreateEvent processes the create-event form POST and redirects back.
func (h *CalendarHandler) HandleCreateEvent(c *fiber.Ctx) error {
	summary := c.FormValue("summary")
	if summary == "" {
		return c.Status(400).JSON(fiber.Map{"error": "summary is required"})
	}

	startStr := c.FormValue("start") // "2006-01-02T15:04"
	endStr := c.FormValue("end")
	allDayStr := c.FormValue("all_day")

	allDay := allDayStr == "true" || allDayStr == "1" || allDayStr == "on"

	var startTime, endTime time.Time
	var parseErr error

	if allDay {
		startTime, parseErr = time.ParseInLocation("2006-01-02", c.FormValue("start_date"), time.Local)
		if parseErr != nil {
			startTime = time.Now()
		}
		endTime = startTime.AddDate(0, 0, 1)
	} else {
		startTime, parseErr = time.ParseInLocation("2006-01-02T15:04", startStr, time.Local)
		if parseErr != nil {
			startTime = time.Now()
		}
		endTime, parseErr = time.ParseInLocation("2006-01-02T15:04", endStr, time.Local)
		if parseErr != nil || endTime.Before(startTime) {
			endTime = startTime.Add(time.Hour)
		}
	}

	ev := models.CalendarEvent{
		Summary:     summary,
		Description: c.FormValue("description"),
		Location:    c.FormValue("location"),
		Start:       startTime,
		End:         endTime,
		AllDay:      allDay,
	}

	client, err := h.caldavClient(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "calendar not available: " + err.Error()})
	}

	ctx := context.Background()
	if err := client.CreateEvent(ctx, ev); err != nil {
		log.Printf("calendar: create event error: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "failed to create event: " + err.Error()})
	}

	// Redirect to month view after successful creation.
	return c.Redirect(fmt.Sprintf("/calendar?year=%d&month=%d", startTime.Year(), int(startTime.Month())))
}

// HandleRSVP sends an iTIP REPLY email to the event organiser.  It builds a
// minimal METHOD:REPLY iCalendar body and delivers it via the session's SMTP
// client, giving real RSVP semantics (RFC 5546).
func (h *CalendarHandler) HandleRSVP(c *fiber.Ctx) error {
	status := c.FormValue("status")       // "accepted", "declined", "tentative"
	uid := c.FormValue("uid")             // iCalendar UID of the event
	organizer := c.FormValue("organizer") // MAILTO: of the organiser

	if uid == "" {
		return c.Status(400).JSON(fiber.Map{"error": "uid is required"})
	}

	partstat := map[string]string{
		"accepted":  "ACCEPTED",
		"declined":  "DECLINED",
		"tentative": "TENTATIVE",
	}[status]
	if partstat == "" {
		return c.Status(400).JSON(fiber.Map{"error": "status must be accepted, declined, or tentative"})
	}

	// Derive attendee email from session.
	sess, err := h.store.Get(c)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "session error"})
	}
	attendeeEmail, _ := sess.Get("email").(string)
	if attendeeEmail == "" {
		return c.Status(401).JSON(fiber.Map{"error": "not authenticated"})
	}

	// Build a minimal METHOD:REPLY iCalendar payload.
	now := time.Now().UTC().Format("20060102T150405Z")
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//LilMail//LilMail//EN",
		"METHOD:REPLY",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + now,
		"ATTENDEE;PARTSTAT=" + partstat + ":MAILTO:" + attendeeEmail,
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	// Determine recipient: use the submitted organizer, or default to sender.
	to := organizer
	if to == "" {
		to = attendeeEmail // fallback — CalDAV server may handle routing
	}
	// Strip MAILTO: prefix if present.
	to = strings.TrimPrefix(to, "MAILTO:")
	to = strings.TrimPrefix(to, "mailto:")

	subject := map[string]string{
		"ACCEPTED":  "Accepted",
		"DECLINED":  "Declined",
		"TENTATIVE": "Tentative",
	}[partstat] + ": " + uid

	smtpClient, err := h.auth.CreateSMTPClient(c)
	if err != nil {
		log.Printf("calendar: RSVP smtp client: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "failed to connect to mail server"})
	}

	opts := &api.MailOptions{}
	if err := smtpClient.SendMail(to, subject, ics, opts); err != nil {
		log.Printf("calendar: RSVP send: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "failed to send RSVP: " + err.Error()})
	}

	label := map[string]string{
		"ACCEPTED":  "Accepted",
		"DECLINED":  "Declined",
		"TENTATIVE": "Tentative",
	}[partstat]
	log.Printf("calendar: RSVP uid=%q partstat=%s sent to %s", uid, partstat, to)
	return c.JSON(fiber.Map{"ok": true, "message": label})
}

// ─── Month grid helpers ────────────────────────────────────────────────────

// GridDay represents one cell in the month grid.
type GridDay struct {
	Date           time.Time
	IsCurrentMonth bool
	IsToday        bool
	Events         []models.CalendarEvent
}

// buildMonthGrid returns a 6-row × 7-column grid (42 GridDay entries) for the
// given year/month, padded with days from adjacent months.  Events are
// distributed into the appropriate cells.
func buildMonthGrid(year int, month time.Month, events []models.CalendarEvent) []GridDay {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	today := time.Now()

	// Offset to Monday-based week (Go's time.Weekday is Sunday=0).
	startOffset := int(first.Weekday()) - int(time.Monday)
	if startOffset < 0 {
		startOffset += 7
	}

	gridStart := first.AddDate(0, 0, -startOffset)

	grid := make([]GridDay, 42)
	for i := range grid {
		d := gridStart.AddDate(0, 0, i)
		isToday := d.Year() == today.Year() && d.YearDay() == today.YearDay()
		grid[i] = GridDay{
			Date:           d,
			IsCurrentMonth: d.Month() == month,
			IsToday:        isToday,
			Events:         eventsOnDay(events, d),
		}
	}
	return grid
}

// ─── Week view helpers ─────────────────────────────────────────────────────

// WeekDay is one column in the week view.
type WeekDay struct {
	Date   time.Time
	Events []models.CalendarEvent
}

// eventsOnDay filters events that overlap with the given day.
func eventsOnDay(events []models.CalendarEvent, day time.Time) []models.CalendarEvent {
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	dayEnd := dayStart.AddDate(0, 0, 1)
	var result []models.CalendarEvent
	for _, ev := range events {
		if ev.End.After(dayStart) && ev.Start.Before(dayEnd) {
			result = append(result, ev)
		}
	}
	return result
}
