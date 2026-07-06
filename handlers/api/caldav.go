// handlers/api/caldav.go — CalDAV client helpers for LilMail.
//
// This file contains the CalDAV client logic: building an authenticated HTTP
// client, discovering calendars, listing events in a time range, and creating
// new events via PutCalendarObject.
//
// NOTE: network calls cannot be exercised without a live CalDAV server; the
// logic is architecturally correct and compile-tested but not end-to-end
// tested in this environment.
package api

import (
	"context"
	"fmt"
	"lilmail/config"
	"lilmail/models"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// bearerHTTPClient wraps an http.Client (or any webdav.HTTPClient) and injects
// an Authorization: Bearer <token> header on every request.
type bearerHTTPClient struct {
	inner webdav.HTTPClient
	token string
}

func (b *bearerHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.inner.Do(req)
}

// newHTTPClient builds an authenticated webdav.HTTPClient according to the
// [caldav] config block.
//   - auth = "basic"  → uses go-webdav's BasicAuth helper.
//   - auth = "oauth2" → injects a bearer token (the caller must supply it).
func newHTTPClient(cfg config.CalDAVConfig, bearerToken string) webdav.HTTPClient {
	// Use the SSRF-hardened client (redirect re-validation + per-dial IP screening
	// / rebind pinning) rather than http.DefaultClient. See dav_url.go.
	base := webdav.HTTPClient(safeDAVHTTPClient())
	if cfg.Auth == "oauth2" {
		if bearerToken != "" {
			return &bearerHTTPClient{inner: base, token: bearerToken}
		}
		return base
	}
	// default: basic auth
	return webdav.HTTPClientWithBasicAuth(base, cfg.Username, cfg.Password)
}

// CalDAVClient wraps the go-webdav caldav.Client with LilMail-specific helpers.
type CalDAVClient struct {
	c    *caldav.Client
	cfg  config.CalDAVConfig
	home string // calendar home-set path (resolved once on first use)
}

// NewCalDAVClient creates a CalDAVClient.  bearerToken is only used when
// cfg.Auth == "oauth2"; it is ignored otherwise.
func NewCalDAVClient(cfg config.CalDAVConfig, bearerToken string) (*CalDAVClient, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("caldav: integration is disabled")
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("caldav: URL is required")
	}
	// SSRF / token-exfil guard: validate the (possibly header-injected) URL before
	// building the client or attaching any bearer token. See dav_url.go.
	if err := validateDAVURL(cfg.URL); err != nil {
		return nil, err
	}
	hc := newHTTPClient(cfg, bearerToken)
	c, err := caldav.NewClient(hc, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("caldav: failed to create client: %w", err)
	}
	return &CalDAVClient{c: c, cfg: cfg}, nil
}

// calendarHome discovers (and caches) the calendar home-set path for the
// current principal.
func (cc *CalDAVClient) calendarHome(ctx context.Context) (string, error) {
	if cc.home != "" {
		return cc.home, nil
	}
	// Discover the current user principal first.
	principal, err := cc.c.FindCurrentUserPrincipal(ctx)
	if err != nil {
		// Some servers don't support current-user-principal; fall back to the
		// configured URL itself as the home-set.
		cc.home = cc.cfg.URL
		return cc.home, nil
	}
	home, err := cc.c.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return "", fmt.Errorf("caldav: find calendar home-set: %w", err)
	}
	cc.home = home
	return cc.home, nil
}

// Calendars returns the list of calendars in the user's home-set.
func (cc *CalDAVClient) Calendars(ctx context.Context) ([]caldav.Calendar, error) {
	home, err := cc.calendarHome(ctx)
	if err != nil {
		return nil, err
	}
	cals, err := cc.c.FindCalendars(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("caldav: find calendars: %w", err)
	}
	return cals, nil
}

// firstVEVENTCalendar returns the path of the first calendar that supports
// VEVENT, or falls back to the calendar home-set.
func (cc *CalDAVClient) firstVEVENTCalendar(ctx context.Context) (string, error) {
	cals, err := cc.Calendars(ctx)
	if err != nil {
		return "", err
	}
	for _, cal := range cals {
		for _, comp := range cal.SupportedComponentSet {
			if comp == ical.CompEvent {
				return cal.Path, nil
			}
		}
	}
	if len(cals) > 0 {
		return cals[0].Path, nil
	}
	// last resort: use home
	home, err := cc.calendarHome(ctx)
	return home, err
}

// ListEvents performs a calendar-query REPORT for VEVENTs between start and
// end, then parses each iCal object into a models.CalendarEvent.
func (cc *CalDAVClient) ListEvents(ctx context.Context, start, end time.Time) ([]models.CalendarEvent, error) {
	calPath, err := cc.firstVEVENTCalendar(ctx)
	if err != nil {
		return nil, err
	}

	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			Comps: []caldav.CalendarCompRequest{
				{
					Name:     ical.CompEvent,
					AllProps: true,
				},
			},
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{
				{
					Name:  ical.CompEvent,
					Start: start,
					End:   end,
				},
			},
		},
	}

	objects, err := cc.c.QueryCalendar(ctx, calPath, query)
	if err != nil {
		return nil, fmt.Errorf("caldav: query calendar: %w", err)
	}

	events := make([]models.CalendarEvent, 0, len(objects))
	for _, obj := range objects {
		if obj.Data == nil {
			continue
		}
		for _, ev := range obj.Data.Events() {
			ce, err := calEventFromICal(obj.Path, ev)
			if err != nil {
				// skip unparseable events rather than aborting the whole list
				continue
			}
			events = append(events, ce)
		}
	}
	return events, nil
}

// calEventFromICal converts a go-ical Event into a models.CalendarEvent.
func calEventFromICal(objPath string, ev ical.Event) (models.CalendarEvent, error) {
	uid, _ := ev.Props.Text(ical.PropUID)
	summary, _ := ev.Props.Text(ical.PropSummary)
	description, _ := ev.Props.Text(ical.PropDescription)
	location, _ := ev.Props.Text(ical.PropLocation)

	organizer := ""
	if orgProp := ev.Props.Get(ical.PropOrganizer); orgProp != nil {
		organizer = orgProp.Value
	}

	startTime, err := ev.DateTimeStart(time.Local)
	if err != nil {
		return models.CalendarEvent{}, fmt.Errorf("caldav: parse DTSTART: %w", err)
	}

	endTime, err := ev.DateTimeEnd(time.Local)
	if err != nil {
		// Non-fatal: use start as end
		endTime = startTime
	}

	// Detect all-day events: DTSTART with VALUE=DATE has no time component.
	allDay := false
	if dtsProp := ev.Props.Get(ical.PropDateTimeStart); dtsProp != nil {
		allDay = dtsProp.ValueType() == ical.ValueDate
	}

	// Recurrence: expose the raw RRULE so the client can display "repeats" and
	// round-trip the rule on edit. We do not expand occurrences server-side.
	recurrence := ""
	if rr := ev.Props.Get(ical.PropRecurrenceRule); rr != nil {
		recurrence = rr.Value
	}

	sequence := 0
	if seq := ev.Props.Get(ical.PropSequence); seq != nil {
		if n, err := seq.Int(); err == nil {
			sequence = n
		}
	}

	// Attendees round-trip so a listed meeting shows who was invited and their
	// current PARTSTAT (updated as REPLYs are processed).
	var attendees []models.Attendee
	for _, prop := range ev.Props.Values(ical.PropAttendee) {
		addr := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(prop.Value), "mailto:"), "MAILTO:")
		if addr == "" {
			continue
		}
		a := models.Attendee{Email: addr}
		if p := prop.Params.Get(ical.ParamParticipationStatus); p != "" {
			a.PartStat = strings.ToUpper(p)
		}
		if r := prop.Params.Get(ical.ParamRole); r != "" {
			a.Role = r
		}
		if cn := prop.Params.Get(ical.ParamCommonName); cn != "" {
			a.Name = cn
		}
		attendees = append(attendees, a)
	}

	return models.CalendarEvent{
		UID:         uid,
		Summary:     summary,
		Description: description,
		Location:    location,
		Organizer:   organizer,
		Start:       startTime,
		End:         endTime,
		AllDay:      allDay,
		Recurrence:  recurrence,
		Path:        objPath,
		Attendees:   attendees,
		Sequence:    sequence,
	}, nil
}

// CreateEvent builds a minimal VCALENDAR/VEVENT and PUTs it on the server.
func (cc *CalDAVClient) CreateEvent(ctx context.Context, ev models.CalendarEvent) error {
	if ev.UID == "" {
		// Generate a simple UID based on the start time
		ev.UID = fmt.Sprintf("lilmail-%d@lilmail", ev.Start.UnixNano())
	}
	return cc.putEvent(ctx, ev)
}

// UpdateEvent overwrites an existing event. CalDAV updates are idempotent PUTs to
// the object's path, so this rebuilds the VEVENT from ev and PUTs it back. When
// ev.Path is set (as returned by ListEvents) the update targets that exact
// object; otherwise it falls back to <calendar>/<uid>.ics like CreateEvent. A UID
// is required — without a stable identity there is nothing to update.
func (cc *CalDAVClient) UpdateEvent(ctx context.Context, ev models.CalendarEvent) error {
	if ev.UID == "" {
		return fmt.Errorf("caldav: update requires an event UID")
	}
	return cc.putEvent(ctx, ev)
}

// putEvent serialises ev to a VCALENDAR/VEVENT and PUTs it. The target object
// path is ev.Path when known (edits), else <firstVEVENTCalendar>/<uid>.ics.
func (cc *CalDAVClient) putEvent(ctx context.Context, ev models.CalendarEvent) error {
	objPath := ev.Path
	if objPath == "" {
		calPath, err := cc.firstVEVENTCalendar(ctx)
		if err != nil {
			return err
		}
		objPath = path.Join(calPath, ev.UID+".ics")
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//LilMail//LilMail//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, ev.UID)
	event.Props.SetText(ical.PropSummary, ev.Summary)
	if ev.Description != "" {
		event.Props.SetText(ical.PropDescription, ev.Description)
	}
	if ev.Location != "" {
		event.Props.SetText(ical.PropLocation, ev.Location)
	}
	if ev.Recurrence != "" {
		event.Props.SetText(ical.PropRecurrenceRule, ev.Recurrence)
	}
	if ev.Sequence > 0 {
		seqProp := ical.NewProp(ical.PropSequence)
		seqProp.SetText(fmt.Sprintf("%d", ev.Sequence))
		event.Props.Set(seqProp)
	}

	// Meeting properties: ORGANIZER + one ATTENDEE per invitee. Written whenever
	// the event carries attendees (an iTIP meeting) OR an explicit organizer, so
	// the stored CalDAV object round-trips the scheduling identity that the
	// mailed REQUEST advertised. go-ical escapes property values on encode.
	if ev.Organizer != "" {
		org := ical.NewProp(ical.PropOrganizer)
		org.Value = "mailto:" + strings.TrimPrefix(strings.TrimPrefix(ev.Organizer, "mailto:"), "MAILTO:")
		event.Props.Set(org)
	}
	for _, a := range ev.Attendees {
		addr := strings.TrimSpace(a.Email)
		if addr == "" {
			continue
		}
		part := a.PartStat
		if part == "" {
			part = "NEEDS-ACTION"
		}
		att := ical.NewProp(ical.PropAttendee)
		att.Params.Set(ical.ParamParticipationStatus, part)
		att.Params.Set("RSVP", "TRUE")
		if a.Role != "" {
			att.Params.Set(ical.ParamRole, a.Role)
		}
		if a.Name != "" {
			att.Params.Set(ical.ParamCommonName, a.Name)
		}
		att.Value = "mailto:" + strings.TrimPrefix(strings.TrimPrefix(addr, "mailto:"), "MAILTO:")
		event.Props.Add(att)
	}

	// Stamp
	stampProp := ical.NewProp(ical.PropDateTimeStamp)
	stampProp.SetDateTime(time.Now().UTC())
	event.Props.Set(stampProp)

	if ev.AllDay {
		event.Props.SetDate(ical.PropDateTimeStart, ev.Start)
		event.Props.SetDate(ical.PropDateTimeEnd, ev.End)
	} else {
		event.Props.SetDateTime(ical.PropDateTimeStart, ev.Start)
		event.Props.SetDateTime(ical.PropDateTimeEnd, ev.End)
	}

	cal.Children = append(cal.Children, event.Component)

	if _, err := cc.c.PutCalendarObject(ctx, objPath, cal); err != nil {
		return fmt.Errorf("caldav: put calendar object: %w", err)
	}
	return nil
}

// DeleteEvent removes the calendar object identified by uid. CalDAV objects are
// addressed by an opaque server path, so we first locate the event in a wide
// time window to discover its path, then DELETE it via the underlying WebDAV
// client. Returns an error wrapping "not found" when no matching event exists.
func (cc *CalDAVClient) DeleteEvent(ctx context.Context, uid string) error {
	if uid == "" {
		return fmt.Errorf("caldav: empty event UID")
	}

	// Search a wide window (±5 years) to find the object path for this UID.
	now := time.Now()
	start := now.AddDate(-5, 0, 0)
	end := now.AddDate(5, 0, 0)

	events, err := cc.ListEvents(ctx, start, end)
	if err != nil {
		return err
	}

	objPath := ""
	for _, ev := range events {
		if ev.UID == uid {
			objPath = ev.Path
			break
		}
	}
	if objPath == "" {
		return fmt.Errorf("caldav: event %q not found", uid)
	}

	// caldav.Client embeds *webdav.Client, which exposes RemoveAll (HTTP DELETE).
	if err := cc.c.RemoveAll(ctx, objPath); err != nil {
		return fmt.Errorf("caldav: delete calendar object: %w", err)
	}
	return nil
}

// FreeBusy returns the busy intervals between start and end, derived from the
// user's calendar events. go-webdav v0.7.0 has no free-busy REPORT helper, so
// we compute busy blocks from the event list and merge overlapping intervals.
// All-day events are skipped (they do not block specific times).
func (cc *CalDAVClient) FreeBusy(ctx context.Context, start, end time.Time) ([]models.FreeBusySlot, error) {
	events, err := cc.ListEvents(ctx, start, end)
	if err != nil {
		return nil, err
	}

	// Collect timed busy intervals, clamped to the requested range.
	slots := make([]models.FreeBusySlot, 0, len(events))
	for _, ev := range events {
		if ev.AllDay {
			continue
		}
		s, e := ev.Start, ev.End
		if !e.After(s) {
			continue
		}
		if s.Before(start) {
			s = start
		}
		if e.After(end) {
			e = end
		}
		if e.After(s) {
			slots = append(slots, models.FreeBusySlot{Start: s, End: e})
		}
	}

	return mergeBusySlots(slots), nil
}

// mergeBusySlots sorts slots by start time and merges any that overlap or touch,
// producing a minimal set of non-overlapping busy intervals.
func mergeBusySlots(slots []models.FreeBusySlot) []models.FreeBusySlot {
	if len(slots) <= 1 {
		return slots
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i].Start.Before(slots[j].Start)
	})
	merged := []models.FreeBusySlot{slots[0]}
	for _, s := range slots[1:] {
		last := &merged[len(merged)-1]
		if !s.Start.After(last.End) { // overlaps or touches
			if s.End.After(last.End) {
				last.End = s.End
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}
