package api

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lilmail/models"

	"github.com/emersion/go-ical"
)

// roundTrip encodes an event to iCal via buildEventCalendar, decodes it, and
// re-parses the first VEVENT back into a CalendarEvent — the same encode/decode
// pair the CalDAV PUT/LIST path uses, minus the network.
func roundTrip(t *testing.T, ev models.CalendarEvent) (string, models.CalendarEvent) {
	t.Helper()
	cal := buildEventCalendar(ev)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := ical.NewDecoder(bytes.NewReader(buf.Bytes())).Decode()
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	var vevent *ical.Component
	for _, c := range decoded.Children {
		if c.Name == ical.CompEvent {
			vevent = c
			break
		}
	}
	if vevent == nil {
		t.Fatalf("no VEVENT:\n%s", buf.String())
	}
	got, err := calEventFromICal("/cal/x.ics", ical.Event{Component: vevent})
	if err != nil {
		t.Fatalf("calEventFromICal: %v", err)
	}
	return buf.String(), got
}

func TestReminderRoundTrip(t *testing.T) {
	start := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	ev := models.CalendarEvent{
		UID: "a1", Summary: "Dentist", Start: start, End: start.Add(time.Hour),
		Reminders: []models.Reminder{
			{Action: "DISPLAY", OffsetMinutes: 15},
			{Action: "EMAIL", OffsetMinutes: 1440, Description: "Bring forms"},
		},
	}
	ics, got := roundTrip(t, ev)
	if !strings.Contains(ics, "BEGIN:VALARM") {
		t.Fatalf("no VALARM:\n%s", ics)
	}
	if len(got.Reminders) != 2 {
		t.Fatalf("reminders = %d, want 2: %+v", len(got.Reminders), got.Reminders)
	}
	if got.Reminders[0].OffsetMinutes != 15 || got.Reminders[0].Action != "DISPLAY" {
		t.Errorf("reminder[0] = %+v", got.Reminders[0])
	}
	if got.Reminders[1].OffsetMinutes != 1440 || got.Reminders[1].Action != "EMAIL" {
		t.Errorf("reminder[1] = %+v", got.Reminders[1])
	}
}

func TestReminderCRLFFolded(t *testing.T) {
	start := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	ev := models.CalendarEvent{
		UID: "a", Summary: "S", Start: start, End: start.Add(time.Hour),
		Reminders: []models.Reminder{{Action: "EMAIL", OffsetMinutes: 10, Description: "Ping\r\nBcc: x@y"}},
	}
	ics, got := roundTrip(t, ev)
	if strings.Contains(ics, "Bcc:") && strings.Contains(ics, "\r\nBcc:") {
		// A raw injected header line would appear as its own folded prop; the
		// guard folds CR/LF to spaces so the value stays on one line.
	}
	if len(got.Reminders) != 1 {
		t.Fatalf("reminders lost: %+v", got.Reminders)
	}
	if strings.ContainsAny(got.Reminders[0].Description, "\r\n") {
		t.Errorf("reminder description still carries CRLF: %q", got.Reminders[0].Description)
	}
}

func TestTZIDRoundTrip(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata: %v", err)
	}
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, ny) // 13:00Z (EDT)
	ev := models.CalendarEvent{
		UID: "tz", Summary: "Call", Start: start, End: start.Add(time.Hour),
		Timezone: "America/New_York",
	}
	ics, got := roundTrip(t, ev)
	if !strings.Contains(ics, "BEGIN:VTIMEZONE") || !strings.Contains(ics, "TZID:America/New_York") {
		t.Fatalf("no VTIMEZONE/TZID:\n%s", ics)
	}
	if got.Timezone != "America/New_York" {
		t.Errorf("timezone = %q", got.Timezone)
	}
	if !got.Start.UTC().Equal(start.UTC()) {
		t.Errorf("start instant = %v, want %v", got.Start.UTC(), start.UTC())
	}
}

func TestTZDSTCorrectness(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata: %v", err)
	}
	for _, tc := range []struct {
		name    string
		start   time.Time
		wantUTC time.Time
	}{
		{"winter EST", time.Date(2026, 1, 15, 9, 0, 0, 0, ny), time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC)},
		{"summer EDT", time.Date(2026, 7, 15, 9, 0, 0, 0, ny), time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := models.CalendarEvent{UID: "d", Summary: "s", Start: tc.start, End: tc.start.Add(time.Hour), Timezone: "America/New_York"}
			_, got := roundTrip(t, ev)
			if !got.Start.UTC().Equal(tc.wantUTC) {
				t.Errorf("start = %v, want %v", got.Start.UTC(), tc.wantUTC)
			}
		})
	}
}

func TestBadTZIDFailsSafe(t *testing.T) {
	if loadZone("Not/AZone") != time.UTC {
		t.Error("unknown TZID not UTC-safe")
	}
	start := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	ev := models.CalendarEvent{UID: "b", Summary: "S", Start: start, End: start.Add(time.Hour), Timezone: "Not/AZone"}
	// A bad zone must not break encode; it degrades to UTC.
	if _, got := roundTrip(t, ev); !got.Start.UTC().Equal(start) {
		t.Errorf("bad TZID broke the event: %v", got.Start)
	}
}

func TestExDateAndRecurrenceIDRoundTrip(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	rid := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	ev := models.CalendarEvent{
		UID: "s", Summary: "Daily", Start: start, End: start.Add(30 * time.Minute),
		Recurrence: "FREQ=DAILY;BYDAY=MO,WE,FR",
		ExDates:    []time.Time{time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)},
	}
	ics, got := roundTrip(t, ev)
	// RRULE must survive without the ';'/',' being escaped or VALUE=TEXT'd.
	if !strings.Contains(ics, "RRULE:FREQ=DAILY;BYDAY=MO,WE,FR") {
		t.Fatalf("RRULE corrupted:\n%s", ics)
	}
	if got.Recurrence != "FREQ=DAILY;BYDAY=MO,WE,FR" {
		t.Errorf("recurrence = %q", got.Recurrence)
	}
	if len(got.ExDates) != 1 || !got.ExDates[0].UTC().Equal(ev.ExDates[0]) {
		t.Errorf("exdates = %+v", got.ExDates)
	}

	// RECURRENCE-ID override.
	moved := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	ov := models.CalendarEvent{UID: "s", Summary: "Moved", Start: moved, End: moved.Add(30 * time.Minute), RecurrenceID: &rid}
	_, gotOv := roundTrip(t, ov)
	if gotOv.RecurrenceID == nil || !gotOv.RecurrenceID.UTC().Equal(rid) {
		t.Errorf("recurrence-id = %v", gotOv.RecurrenceID)
	}
}
