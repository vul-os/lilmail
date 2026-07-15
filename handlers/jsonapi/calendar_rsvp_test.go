package jsonapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// recordingCalDAV is a no-network calDAVClient that RECORDS the mutating calls the
// RSVP / update-with-invites paths make, so a test can assert the reflected event
// and deletions without a live CalDAV server.
type recordingCalDAV struct {
	updated []models.CalendarEvent
	deleted []string
	failAll bool
}

func (f *recordingCalDAV) ListEvents(context.Context, time.Time, time.Time) ([]models.CalendarEvent, error) {
	return nil, nil
}
func (f *recordingCalDAV) CreateEvent(_ context.Context, ev models.CalendarEvent) error {
	if f.failAll {
		return context.DeadlineExceeded
	}
	f.updated = append(f.updated, ev)
	return nil
}
func (f *recordingCalDAV) UpdateEvent(_ context.Context, ev models.CalendarEvent) error {
	if f.failAll {
		return context.DeadlineExceeded
	}
	f.updated = append(f.updated, ev)
	return nil
}
func (f *recordingCalDAV) DeleteEvent(_ context.Context, uid string) error {
	if f.failAll {
		return context.DeadlineExceeded
	}
	f.deleted = append(f.deleted, uid)
	return nil
}
func (f *recordingCalDAV) FreeBusy(context.Context, time.Time, time.Time) ([]models.FreeBusySlot, error) {
	return nil, nil
}

// rsvpRequest builds a brokered POST /v1/calendar/rsvp request with the given body.
func rsvpRequest(body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/calendar/rsvp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCalDAVURL, "https://dav.example.com/caldav/")
	return req
}

// Accepting an invitation must (1) mail a METHOD:REPLY to the organizer and
// (2) reflect the event in the responder's own calendar with their PARTSTAT.
func TestCalendarRSVPAcceptSendsReplyAndReflects(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	cal := &recordingCalDAV{}
	withStubbedCalDAVDial(t, cal, nil)

	cap := &captureSMTP{}
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{
		"uid":"evt-42",
		"organizer":"mailto:boss@corp.example",
		"response":"accept",
		"summary":"Planning",
		"start":"2026-08-01T15:00:00Z",
		"end":"2026-08-01T16:00:00Z",
		"sequence":2
	}`
	resp, err := app.Test(rsvpRequest(body), 5000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	var out struct {
		OK              bool   `json:"ok"`
		PartStat        string `json:"partStat"`
		ReplySent       bool   `json:"replySent"`
		CalendarUpdated bool   `json:"calendarUpdated"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad JSON: %s", b)
	}
	if !out.OK || out.PartStat != "ACCEPTED" || !out.ReplySent || !out.CalendarUpdated {
		t.Fatalf("unexpected rsvp result: %+v (%s)", out, b)
	}

	// A REPLY iMIP must have been mailed to the organizer only.
	if len(cap.rcpts) != 1 || cap.rcpts[0] != "boss@corp.example" {
		t.Fatalf("reply not addressed to organizer: %v", cap.rcpts)
	}
	raw := string(cap.raw)
	// The ICS travels as a base64 text/calendar part; the method is carried
	// unencoded in the part's Content-Type, and the PARTSTAT round-trips through the
	// decoded body.
	if !strings.Contains(raw, "method=REPLY") {
		t.Fatalf("reply iMIP not a METHOD=REPLY calendar part:\n%s", raw)
	}
	if ics := decodeCalendarPart(t, raw); !strings.Contains(ics, "METHOD:REPLY") || !strings.Contains(ics, "PARTSTAT=ACCEPTED") {
		t.Fatalf("decoded reply ICS missing REPLY/PARTSTAT:\n%s", ics)
	}

	// The event must have been reflected into the responder's calendar with their
	// PARTSTAT recorded.
	if len(cal.updated) != 1 {
		t.Fatalf("event not reflected into calendar: %d updates", len(cal.updated))
	}
	ev := cal.updated[0]
	if ev.UID != "evt-42" || len(ev.Attendees) != 1 || ev.Attendees[0].PartStat != "ACCEPTED" {
		t.Fatalf("reflected event wrong: %+v", ev)
	}
	if ev.Attendees[0].Email != "user@gmail.com" {
		t.Fatalf("reflected attendee is not the authenticated identity: %+v", ev.Attendees[0])
	}
}

// Declining must remove any prior calendar copy and still report success. With no
// organizer no REPLY is mailed (replySent=false) but the decline is recorded.
func TestCalendarRSVPDeclineRemovesCopyNoOrganizer(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	cal := &recordingCalDAV{}
	withStubbedCalDAVDial(t, cal, nil)

	cap := &captureSMTP{}
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// No organizer → no REPLY mail, but the decline still reflects (removes copy).
	body := `{"uid":"evt-9","response":"decline","summary":"Skip"}`
	resp, err := app.Test(rsvpRequest(body), 5000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	var out struct {
		PartStat        string `json:"partStat"`
		ReplySent       bool   `json:"replySent"`
		CalendarUpdated bool   `json:"calendarUpdated"`
	}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	if out.PartStat != "DECLINED" || out.ReplySent {
		t.Fatalf("decline w/o organizer should not send a reply: %+v", out)
	}
	if !out.CalendarUpdated || len(cal.deleted) != 1 || cal.deleted[0] != "evt-9" {
		t.Fatalf("decline did not remove the calendar copy: deleted=%v", cal.deleted)
	}
	if len(cap.raw) != 0 {
		t.Fatalf("no mail should be sent when there is no organizer, got %d bytes", len(cap.raw))
	}
}

// A malformed response verb and a missing UID must both fail closed with 400 and
// never touch the mail server or the calendar.
func TestCalendarRSVPRejectsBadInput(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	cal := &recordingCalDAV{}
	withStubbedCalDAVDial(t, cal, nil)

	smtpCalled := false
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender {
		smtpCalled = true
		return &captureSMTP{}
	}
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	cases := []struct{ name, body string }{
		{"unknown response", `{"uid":"e1","organizer":"mailto:o@x.com","response":"maybe"}`},
		{"missing uid", `{"organizer":"mailto:o@x.com","response":"accept"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := app.Test(rsvpRequest(tc.body), 5000)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("want 400, got %d", resp.StatusCode)
			}
		})
	}
	if smtpCalled {
		t.Fatalf("mail server contacted for an invalid RSVP")
	}
	if len(cal.updated) != 0 || len(cal.deleted) != 0 {
		t.Fatalf("calendar mutated for an invalid RSVP: %+v %+v", cal.updated, cal.deleted)
	}
}

// Organizer editing a meeting (PUT with attendees) must persist the event AND
// re-send METHOD:REQUEST invites to every attendee, reporting the invited count.
func TestCalendarUpdateWithAttendeesResendsInvites(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	cal := &recordingCalDAV{}
	withStubbedCalDAVDial(t, cal, nil)

	// Record every recipient across the per-attendee sends.
	var allRcpts []string
	var lastRaw string
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender {
		return smtpFunc(func(rcpts []string, raw []byte) error {
			allRcpts = append(allRcpts, rcpts...)
			lastRaw = string(raw)
			return nil
		})
	}
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	body := `{
		"summary":"Design sync",
		"start":"2026-08-10T09:00:00Z",
		"end":"2026-08-10T10:00:00Z",
		"location":"Room 4",
		"attendees":[{"email":"a@team.example"},{"email":"b@team.example"}]
	}`
	req := httptest.NewRequest("PUT", "/v1/calendar/events/evt-77", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCalDAVURL, "https://dav.example.com/caldav/")

	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Updated bool `json:"updated"`
		Invited int  `json:"invited"`
	}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	if !out.Updated || out.Invited != 2 {
		t.Fatalf("want {updated:true, invited:2}, got %+v (%s)", out, b)
	}
	if len(cal.updated) != 1 {
		t.Fatalf("event not persisted: %d updates", len(cal.updated))
	}
	if !containsFold(allRcpts, "a@team.example") || !containsFold(allRcpts, "b@team.example") {
		t.Fatalf("invites not mailed to all attendees: %v", allRcpts)
	}
	// The iMIP part is a METHOD=REQUEST calendar object; the decoded ICS carries the
	// event summary.
	if !strings.Contains(lastRaw, "method=REQUEST") {
		t.Fatalf("invite not a METHOD=REQUEST calendar part:\n%s", lastRaw)
	}
	if ics := decodeCalendarPart(t, lastRaw); !strings.Contains(ics, "METHOD:REQUEST") || !strings.Contains(ics, "Design sync") {
		t.Fatalf("decoded invite missing REQUEST/summary:\n%s", ics)
	}
}

// decodeCalendarPart extracts and base64-decodes the text/calendar MIME part from
// a raw iMIP message so a test can assert on the ICS content itself.
func decodeCalendarPart(t *testing.T, raw string) string {
	t.Helper()
	idx := strings.Index(raw, "text/calendar")
	if idx < 0 {
		t.Fatalf("no text/calendar part:\n%s", raw)
	}
	// The base64 body starts after the part headers (the blank line following them).
	rest := raw[idx:]
	sep := strings.Index(rest, "\r\n\r\n")
	if sep < 0 {
		t.Fatalf("malformed calendar part:\n%s", rest)
	}
	body := rest[sep+4:]
	// The base64 body ends at the next MIME boundary ("--").
	if end := strings.Index(body, "\r\n--"); end >= 0 {
		body = body[:end]
	}
	b64 := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", ""), "\n", "")
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		t.Fatalf("decode calendar base64: %v\nbody=%q", err, b64)
	}
	return string(dec)
}

// Deleting an event by UID must route through the brokered CalDAV client and
// return 204; a missing CalDAV URL must report 501 without touching the session.
func TestCalendarDeleteEvent(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	cal := &recordingCalDAV{}
	withStubbedCalDAVDial(t, cal, nil)

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// (1) With a CalDAV URL → 204, delete recorded.
	req := httptest.NewRequest("DELETE", "/v1/calendar/events/evt-5", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCalDAVURL, "https://dav.example.com/caldav/")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(cal.deleted) != 1 || cal.deleted[0] != "evt-5" {
		t.Fatalf("delete not routed to CalDAV: %v", cal.deleted)
	}

	// (2) Without a CalDAV URL → 501.
	req2 := httptest.NewRequest("DELETE", "/v1/calendar/events/evt-5", nil)
	req2.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req2.Header.Set(k, v)
	}
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp2.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp2.StatusCode)
	}
}

// smtpFunc adapts a func to the smtpSender interface for per-send capture.
type smtpFunc func(rcpts []string, raw []byte) error

func (f smtpFunc) SendRawMessage(rcpts []string, raw []byte) error { return f(rcpts, raw) }
