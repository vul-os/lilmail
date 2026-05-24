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
	// CalDAV path this object lives at (server-assigned, empty for new events).
	Path string `json:"path,omitempty"`
}
