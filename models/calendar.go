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
	// Timezone is the IANA time-zone id (e.g. "America/New_York") for a timed
	// event. Empty means UTC/floating. When set, Start/End are emitted as local
	// wall-clock times carrying a TZID parameter (with a VTIMEZONE) so the event
	// renders at the correct time across DST and other zones.
	Timezone string `json:"timezone,omitempty"`
	// Reminders are the VALARM alarms on the event (popup + email), each a
	// relative offset before the start. Round-tripped so the client can edit
	// them.
	Reminders []Reminder `json:"reminders,omitempty"`
	// ExDates are excluded occurrence starts (EXDATE): single instances deleted
	// from a recurring series. RFC3339 wall-clock in the event's zone.
	ExDates []time.Time `json:"exdates,omitempty"`
	// RecurrenceID, when non-zero, marks this object as a single-instance
	// override of a recurring series (the occurrence it replaces).
	RecurrenceID *time.Time `json:"recurrenceId,omitempty"`
	// CalDAV path this object lives at (server-assigned, empty for new events).
	Path string `json:"path,omitempty"`
	// Attendees are the invitees on this event (iTIP). When non-empty on create/
	// update, the event is treated as a meeting: an ATTENDEE line (PARTSTAT=
	// NEEDS-ACTION) is written per attendee and a METHOD:REQUEST iMIP invite is
	// mailed to each. Empty for a personal (non-meeting) event.
	Attendees []Attendee `json:"attendees,omitempty"`
	// Sequence is the iTIP SEQUENCE number; bumped on each organizer update so
	// clients can tell a reschedule from a duplicate. Zero for a fresh event.
	Sequence int `json:"sequence,omitempty"`
}

// Reminder is a single VALARM alarm: fire OffsetMinutes before the event start,
// as a popup (DISPLAY) or an email (EMAIL).
type Reminder struct {
	// Action is DISPLAY (popup) or EMAIL. Anything else is normalized to
	// DISPLAY on the server.
	Action string `json:"action"`
	// OffsetMinutes is minutes BEFORE the event start (positive = before).
	OffsetMinutes int `json:"offsetMinutes"`
	// Description is optional alarm text / email body.
	Description string `json:"description,omitempty"`
}

// Attendee is one invitee on a meeting (iTIP ATTENDEE property).
type Attendee struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	// PartStat is the participation status: NEEDS-ACTION, ACCEPTED, DECLINED,
	// TENTATIVE. Defaults to NEEDS-ACTION when the organizer first invites.
	PartStat string `json:"partStat,omitempty"`
	// Role is CHAIR, REQ-PARTICIPANT (default) or OPT-PARTICIPANT.
	Role string `json:"role,omitempty"`
}

// CalendarInvite is the parsed result of an inbound iMIP message: a text/calendar
// part carrying an iTIP METHOD. It is attached to a fetched Email so the mail
// client can render an RSVP card (for REQUEST) or a status line (for REPLY /
// CANCEL) without re-parsing MIME on the client.
type CalendarInvite struct {
	// Method is the iTIP method, upper-cased: REQUEST, REPLY, CANCEL, COUNTER,
	// PUBLISH. The mail-ui shows an RSVP card only for REQUEST.
	Method string `json:"method"`
	// Event fields extracted from the VEVENT.
	UID         string    `json:"uid"`
	Summary     string    `json:"summary,omitempty"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Organizer   string    `json:"organizer,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allDay"`
	Sequence    int       `json:"sequence,omitempty"`
	Recurrence  string    `json:"recurrence,omitempty"`
	// Attendees carries the invitee list (for REQUEST) or the single responding
	// attendee with its new PARTSTAT (for REPLY).
	Attendees []Attendee `json:"attendees,omitempty"`
	// MyPartStat is the current participation status of the viewing user among
	// the attendees, when identifiable; empty otherwise. Lets the card show the
	// already-chosen answer.
	MyPartStat string `json:"myPartStat,omitempty"`
}

// FreeBusySlot is a single busy interval derived from calendar events, used by
// the free/busy endpoint. Slots are always "busy" intervals; any gap between
// them (within the requested range) is implicitly free.
type FreeBusySlot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
