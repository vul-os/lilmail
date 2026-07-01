package jsonapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// fakeCalDAV is a no-network calDAVClient used to assert that the brokered
// calendar path builds a client from the headers without contacting a live
// CalDAV server.
type fakeCalDAV struct {
	events []models.CalendarEvent
	busy   []models.FreeBusySlot
}

func (f *fakeCalDAV) ListEvents(context.Context, time.Time, time.Time) ([]models.CalendarEvent, error) {
	return f.events, nil
}
func (f *fakeCalDAV) CreateEvent(context.Context, models.CalendarEvent) error { return nil }
func (f *fakeCalDAV) UpdateEvent(context.Context, models.CalendarEvent) error { return nil }
func (f *fakeCalDAV) DeleteEvent(context.Context, string) error               { return nil }
func (f *fakeCalDAV) FreeBusy(context.Context, time.Time, time.Time) ([]models.FreeBusySlot, error) {
	return f.busy, nil
}

// withStubbedCalDAVDial swaps brokerDialCalDAV for the duration of a test and
// records the spec it was called with.
func withStubbedCalDAVDial(t *testing.T, cl calDAVClient, captured *brokerSpec) {
	t.Helper()
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(spec brokerSpec) (calDAVClient, error) {
		if captured != nil {
			*captured = spec
		}
		return cl, nil
	}
	t.Cleanup(func() { brokerDialCalDAV = orig })
}

func newBrokerHandler(t *testing.T) *Handler {
	t.Helper()
	store := session.New()
	cfg := &config.Config{}
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	return h
}

// A brokered request carrying a CalDAV URL must build the CalDAV client from the
// header URL + bearer token (via the dial seam) and serve events from it.
func TestBrokeredCalendarBuildsClientFromHeaders(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &fakeCalDAV{events: []models.CalendarEvent{{UID: "evt-1", Summary: "Standup"}}}
	var got brokerSpec
	withStubbedCalDAVDial(t, fake, &got)

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/calendar/events", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCalDAVURL, "https://dav.example.com/caldav/")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	// The spec passed to the dial seam must carry the CalDAV URL + bearer token.
	if got.CalDAVURL != "https://dav.example.com/caldav/" {
		t.Fatalf("CalDAV URL not threaded to dial seam: %+v", got)
	}
	if got.Secret != "ya29.access-token" || got.Auth != "xoauth2" {
		t.Fatalf("bearer token/auth not threaded to dial seam: %+v", got)
	}

	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Events []models.CalendarEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if len(out.Events) != 1 || out.Events[0].UID != "evt-1" {
		t.Fatalf("events not served from brokered CalDAV client: %s", body)
	}
}

// A brokered request WITHOUT a CalDAV URL must return an empty result and never
// invoke the dial seam (nor touch the session).
func TestBrokeredCalendarMissingURLGraceful(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) {
		called = true
		return &fakeCalDAV{}, nil
	}
	t.Cleanup(func() { brokerDialCalDAV = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/calendar/events", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// No X-Vulos-Mail-Caldav-Url header.

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200 empty, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite missing CalDAV URL")
	}

	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Events []models.CalendarEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if out.Events == nil || len(out.Events) != 0 {
		t.Fatalf("want empty events array, got: %s", body)
	}
}

// Creating an event for a brokered account without a CalDAV URL must report a
// 501-style "not available" rather than touching the session.
func TestBrokeredCreateEventMissingURLNotImplemented(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) {
		called = true
		return &fakeCalDAV{}, nil
	}
	t.Cleanup(func() { brokerDialCalDAV = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("POST", "/v1/calendar/events",
		strings.NewReader(`{"summary":"x","start":"2026-06-26T10:00:00Z","end":"2026-06-26T11:00:00Z"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite missing CalDAV URL")
	}
}

// With the WRONG broker secret the DAV URL header must be ignored: no session →
// 401, and the dial seam must not be invoked.
func TestBrokeredCalendarWrongSecretIgnoresHeaders(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	called := false
	orig := brokerDialCalDAV
	brokerDialCalDAV = func(brokerSpec) (calDAVClient, error) {
		called = true
		return &fakeCalDAV{}, nil
	}
	t.Cleanup(func() { brokerDialCalDAV = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/calendar/events", nil)
	req.Header.Set(hdrBrokerAuth, "WRONG")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCalDAVURL, "https://dav.example.com/caldav/")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 (headers ignored, no session), got %d", resp.StatusCode)
	}
	if called {
		t.Fatalf("dial seam invoked despite wrong broker secret — credential trust leak")
	}
}

// Brokered contacts must query CardDAV from the header URL + bearer token (via
// the seam), and a missing CardDAV URL must yield an empty list without calling
// the seam.
func TestBrokeredContacts(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var gotSpec brokerSpec
	seamCalled := false
	orig := brokerCardDAVContacts
	brokerCardDAVContacts = func(spec brokerSpec, query string, limit int) []api.RecipientEntry {
		seamCalled = true
		gotSpec = spec
		return []api.RecipientEntry{{Email: "a@b.com", Name: "Alice"}}
	}
	t.Cleanup(func() { brokerCardDAVContacts = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// (1) With a CardDAV URL → seam invoked, results returned.
	req := httptest.NewRequest("GET", "/v1/contacts?q=al", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCardDAVURL, "https://dav.example.com/carddav/")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !seamCalled || gotSpec.CardDAVURL != "https://dav.example.com/carddav/" || gotSpec.Secret != "ya29.access-token" {
		t.Fatalf("CardDAV seam not wired from headers: called=%v spec=%+v", seamCalled, gotSpec)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Contacts []struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if len(out.Contacts) != 1 || out.Contacts[0].Email != "a@b.com" {
		t.Fatalf("contacts not served from brokered CardDAV: %s", body)
	}

	// (2) Without a CardDAV URL → empty list, seam NOT invoked.
	seamCalled = false
	req2 := httptest.NewRequest("GET", "/v1/contacts?q=al", nil)
	req2.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req2.Header.Set(k, v)
	}
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp2.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp2.StatusCode)
	}
	if seamCalled {
		t.Fatalf("CardDAV seam invoked despite missing CardDAV URL")
	}
	body2, _ := io.ReadAll(resp2.Body)
	if err := json.Unmarshal(body2, &out); err != nil {
		t.Fatalf("bad JSON: %s", body2)
	}
	if len(out.Contacts) != 0 {
		t.Fatalf("want empty contacts, got: %s", body2)
	}
}
