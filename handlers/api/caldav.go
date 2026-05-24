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
	base := webdav.HTTPClient(http.DefaultClient)
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

	return models.CalendarEvent{
		UID:         uid,
		Summary:     summary,
		Description: description,
		Location:    location,
		Organizer:   organizer,
		Start:       startTime,
		End:         endTime,
		AllDay:      allDay,
		Path:        objPath,
	}, nil
}

// CreateEvent builds a minimal VCALENDAR/VEVENT and PUTs it on the server.
func (cc *CalDAVClient) CreateEvent(ctx context.Context, ev models.CalendarEvent) error {
	calPath, err := cc.firstVEVENTCalendar(ctx)
	if err != nil {
		return err
	}

	if ev.UID == "" {
		// Generate a simple UID based on the start time
		ev.UID = fmt.Sprintf("lilmail-%d@lilmail", ev.Start.UnixNano())
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

	// PUT to <calPath>/<uid>.ics
	objPath := path.Join(calPath, ev.UID+".ics")
	_, err = cc.c.PutCalendarObject(ctx, objPath, cal)
	if err != nil {
		return fmt.Errorf("caldav: put calendar object: %w", err)
	}
	return nil
}
