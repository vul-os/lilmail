// handlers/api/caldav_depth.go — calendar "depth" helpers for the CalDAV client:
// per-event timezone (TZID/VTIMEZONE), VALARM reminders, and recurrence
// exceptions (EXDATE / RECURRENCE-ID). Parsing of untrusted server iCal is
// bounded and fail-safe: a malformed alarm, timezone or exception is dropped,
// never propagated as an error that would break event loading.
package api

import (
	"fmt"
	"strings"
	"time"

	"lilmail/models"

	"github.com/emersion/go-ical"
)

// Bounds — a single event cannot carry an unbounded number of alarms or
// exclusions, and an offset cannot exceed a year (guards absurd/overflow input
// from an untrusted server).
const (
	maxRemindersPerEvent = 25
	maxExDatesPerEvent   = 500
	maxOffsetMinutes     = 366 * 24 * 60
	minOffsetMinutes     = -366 * 24 * 60
)

// loadZone resolves an IANA TZID, failing safe to UTC.
func loadZone(tzid string) *time.Location {
	tzid = strings.TrimSpace(tzid)
	if tzid == "" || tzid == "UTC" || len(tzid) > 64 {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tzid); err == nil && loc != nil {
		return loc
	}
	return time.UTC
}

func clampOffset(m int) int {
	if m > maxOffsetMinutes {
		return maxOffsetMinutes
	}
	if m < minOffsetMinutes {
		return minOffsetMinutes
	}
	return m
}

func normalizeAction(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "EMAIL") {
		return "EMAIL"
	}
	return "DISPLAY"
}

// cleanText folds control characters so a value cannot corrupt encoding or forge
// an iCal line / mail header.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// writeTimezone writes DTSTART/DTEND with a TZID parameter and emits a bounded
// VTIMEZONE, when ev is a timed event in a non-UTC zone. Returns true when it
// handled the date properties (so the caller skips its default UTC emit).
func writeTimezone(cal *ical.Calendar, event *ical.Event, ev models.CalendarEvent) bool {
	if ev.AllDay || ev.Timezone == "" {
		return false
	}
	loc := loadZone(ev.Timezone)
	if loc == time.UTC {
		return false
	}
	if vtz := buildVTimezone(loc, ev.Timezone, ev.Start); vtz != nil {
		cal.Children = append(cal.Children, vtz)
	}
	event.Props.SetDateTime(ical.PropDateTimeStart, ev.Start.In(loc))
	event.Props.SetDateTime(ical.PropDateTimeEnd, ev.End.In(loc))
	return true
}

// writeReminders appends a VALARM child per reminder, bounded and sanitized.
func writeReminders(event *ical.Event, reminders []models.Reminder) {
	for i, r := range reminders {
		if i >= maxRemindersPerEvent {
			break
		}
		comp := ical.NewComponent(ical.CompAlarm)
		comp.Props.SetText(ical.PropAction, normalizeAction(r.Action))
		trig := ical.NewProp(ical.PropTrigger)
		trig.SetDuration(time.Duration(-clampOffset(r.OffsetMinutes)) * time.Minute)
		comp.Props.Set(trig)
		desc := cleanText(r.Description)
		if desc == "" {
			desc = "Reminder"
		}
		comp.Props.SetText(ical.PropDescription, desc)
		if normalizeAction(r.Action) == "EMAIL" {
			comp.Props.SetText(ical.PropSummary, desc)
		}
		event.Children = append(event.Children, comp)
	}
}

// writeExceptions writes EXDATE lines and a RECURRENCE-ID override, in the
// event's zone.
func writeExceptions(event *ical.Event, ev models.CalendarEvent) {
	loc := loadZone(ev.Timezone)
	for i, ex := range ev.ExDates {
		if i >= maxExDatesPerEvent {
			break
		}
		if ex.IsZero() {
			continue
		}
		p := ical.NewProp("EXDATE")
		switch {
		case ev.AllDay:
			p.SetDate(ex)
		case loc != time.UTC:
			p.SetDateTime(ex.In(loc))
		default:
			p.SetDateTime(ex.UTC())
		}
		event.Props.Add(p)
	}
	if ev.RecurrenceID != nil && !ev.RecurrenceID.IsZero() {
		p := ical.NewProp(ical.PropRecurrenceID)
		switch {
		case ev.AllDay:
			p.SetDate(*ev.RecurrenceID)
		case loc != time.UTC:
			p.SetDateTime(ev.RecurrenceID.In(loc))
		default:
			p.SetDateTime(ev.RecurrenceID.UTC())
		}
		event.Props.Set(p)
	}
}

// parseDepth extracts TZID, reminders and exceptions from a fetched VEVENT into
// the event model. Fail-safe: individual bad data is skipped.
func parseDepth(comp *ical.Component, dtStart time.Time, out *models.CalendarEvent) {
	// TZID from DTSTART.
	if dts := comp.Props.Get(ical.PropDateTimeStart); dts != nil {
		out.Timezone = strings.TrimSpace(dts.Params.Get(ical.ParamTimezoneID))
	}
	loc := loadZone(out.Timezone)

	// Reminders.
	var reminders []models.Reminder
	for _, child := range comp.Children {
		if child.Name != ical.CompAlarm {
			continue
		}
		if len(reminders) >= maxRemindersPerEvent {
			break
		}
		off, ok := parseTrigger(child.Props.Get(ical.PropTrigger), dtStart)
		if !ok {
			continue
		}
		r := models.Reminder{Action: normalizeAction(compText(child, ical.PropAction)), OffsetMinutes: off}
		r.Description = compText(child, ical.PropDescription)
		reminders = append(reminders, r)
	}
	out.Reminders = reminders

	// EXDATE.
	for _, p := range comp.Props.Values("EXDATE") {
		for i, part := range strings.Split(p.Value, ",") {
			if i >= maxExDatesPerEvent || len(out.ExDates) >= maxExDatesPerEvent {
				break
			}
			sub := p
			sub.Value = strings.TrimSpace(part)
			if sub.Value == "" {
				continue
			}
			if t, err := sub.DateTime(loc); err == nil {
				out.ExDates = append(out.ExDates, t)
			}
		}
	}

	// RECURRENCE-ID.
	if rid := comp.Props.Get(ical.PropRecurrenceID); rid != nil {
		if t, err := rid.DateTime(loc); err == nil {
			tc := t
			out.RecurrenceID = &tc
		}
	}
}

// parseTrigger parses a VALARM TRIGGER into minutes-before-start, fail-safe.
func parseTrigger(p *ical.Prop, dtStart time.Time) (int, bool) {
	if p == nil {
		return 0, false
	}
	if p.ValueType() == ical.ValueDateTime {
		if at, err := p.DateTime(time.UTC); err == nil && !dtStart.IsZero() {
			return clampOffset(int(dtStart.Sub(at).Minutes())), true
		}
		return 0, false
	}
	if d, err := p.Duration(); err == nil {
		return clampOffset(int(-d.Minutes())), true
	}
	return 0, false
}

// eventTZID returns the TZID parameter from a VEVENT's DTSTART, if any.
func eventTZID(comp *ical.Component) string {
	if dts := comp.Props.Get(ical.PropDateTimeStart); dts != nil {
		return strings.TrimSpace(dts.Params.Get(ical.ParamTimezoneID))
	}
	return ""
}

func compText(c *ical.Component, name string) string {
	if p := c.Props.Get(name); p != nil {
		if s, err := p.Text(); err == nil {
			return s
		}
		return p.Value
	}
	return ""
}

// tzOffsetString formats a zone offset (seconds) as +HHMM / +HHMMSS.
func tzOffsetString(offsetSec int) string {
	sign := "+"
	if offsetSec < 0 {
		sign = "-"
		offsetSec = -offsetSec
	}
	h := offsetSec / 3600
	m := (offsetSec % 3600) / 60
	s := offsetSec % 60
	if s != 0 {
		return fmt.Sprintf("%s%02d%02d%02d", sign, h, m, s)
	}
	return fmt.Sprintf("%s%02d%02d", sign, h, m)
}

// buildVTimezone emits a bounded VTIMEZONE (the observance at ref plus, if the
// zone observes DST near ref, the adjacent one). Single backward probe — never
// an unbounded transition scan.
func buildVTimezone(loc *time.Location, tzid string, ref time.Time) *ical.Component {
	if loc == time.UTC || tzid == "" || tzid == "UTC" {
		return nil
	}
	if ref.IsZero() {
		ref = time.Now()
	}
	curName, curOff := ref.In(loc).Zone()
	prev := ref.AddDate(0, 0, -200)
	prevName, prevOff := prev.In(loc).Zone()

	vtz := ical.NewComponent(ical.CompTimezone)
	vtz.Props.SetText(ical.PropTimezoneID, tzid)
	addObservance(vtz, ref.In(loc), curName, prevOff, curOff, curOff >= prevOff)
	if prevOff != curOff {
		addObservance(vtz, prev.In(loc), prevName, curOff, prevOff, prevOff >= curOff)
	}
	return vtz
}

func addObservance(vtz *ical.Component, start time.Time, name string, offsetFrom, offsetTo int, isDaylight bool) {
	compName := ical.CompTimezoneStandard
	if isDaylight {
		compName = ical.CompTimezoneDaylight
	}
	obs := ical.NewComponent(compName)
	dtStart := ical.NewProp(ical.PropDateTimeStart)
	dtStart.SetValueType(ical.ValueDateTime)
	dtStart.Value = start.Format("20060102T150405")
	obs.Props.Set(dtStart)
	obs.Props.SetText(ical.PropTimezoneOffsetFrom, tzOffsetString(offsetFrom))
	obs.Props.SetText(ical.PropTimezoneOffsetTo, tzOffsetString(offsetTo))
	if name != "" && len(name) <= 32 {
		obs.Props.SetText(ical.PropTimezoneName, name)
	}
	vtz.Children = append(vtz.Children, obs)
}
