package models

import "time"

// CalendarEvent represents a single calendar event parsed from CalDAV/iCal.
type CalendarEvent struct {
	UID         string    `json:"uid"`
	Summary     string    `json:"summary"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Organizer   string    `json:"organizer,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allDay"`
	// Recurrence is the raw iCalendar RRULE (e.g. "FREQ=WEEKLY;COUNT=10") when the
	// event repeats, empty for one-off events. Stored/emitted verbatim so the
	// client can round-trip a rule it built without the server understanding every
	// RFC 5545 nuance.
	Recurrence string `json:"recurrence,omitempty"`
	// CalDAV path this object lives at (server-assigned, empty for new events).
	Path string `json:"path,omitempty"`
}

// FreeBusySlot is a single busy interval derived from calendar events, used by
// the free/busy endpoint. Slots are always "busy" intervals; any gap between
// them (within the requested range) is implicitly free.
type FreeBusySlot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
