package jsonapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// Full-card contacts list must query CardDAV from the header URL + bearer token
// via the seam, and a missing URL must yield an empty list without the seam.
func TestBrokeredContactCards(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var gotSpec brokerSpec
	orig := brokerContactsList
	brokerContactsList = func(spec brokerSpec, query string, limit int) ([]models.Contact, error) {
		gotSpec = spec
		return []models.Contact{{UID: "u1", Name: "Alice", Emails: []string{"a@b.com"}, Path: "/ab/u1.vcf"}}, nil
	}
	t.Cleanup(func() { brokerContactsList = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("GET", "/v1/contacts/cards?q=al", nil)
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
	if gotSpec.CardDAVURL != "https://dav.example.com/carddav/" || gotSpec.Secret != "ya29.access-token" {
		t.Fatalf("CardDAV seam not wired from headers: %+v", gotSpec)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Contacts []models.Contact `json:"contacts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if len(out.Contacts) != 1 || out.Contacts[0].UID != "u1" || out.Contacts[0].Path != "/ab/u1.vcf" {
		t.Fatalf("cards not served from brokered CardDAV: %s", body)
	}
}

// Creating a contact must route through the bearer PUT seam and return the saved
// card. A brokered account without a CardDAV URL must report 501.
func TestBrokeredCreateContact(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var got models.Contact
	orig := brokerContactPut
	brokerContactPut = func(spec brokerSpec, ct models.Contact) (models.Contact, error) {
		got = ct
		ct.UID = "new-uid"
		ct.Path = "/ab/new-uid.vcf"
		return ct, nil
	}
	t.Cleanup(func() { brokerContactPut = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// (1) With a CardDAV URL → seam invoked, 201 with saved card.
	req := httptest.NewRequest("POST", "/v1/contacts",
		strings.NewReader(`{"name":"Bob","emails":["bob@x.com"],"phones":["+123"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailCardDAVURL, "https://dav.example.com/carddav/")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, body)
	}
	if got.Name != "Bob" || len(got.Emails) != 1 || got.Emails[0] != "bob@x.com" {
		t.Fatalf("contact not threaded to seam: %+v", got)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Contact models.Contact `json:"contact"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	if out.Contact.UID != "new-uid" {
		t.Fatalf("saved card not returned: %s", body)
	}

	// (2) Without a CardDAV URL → 501, seam NOT invoked.
	called := false
	brokerContactPut = func(spec brokerSpec, ct models.Contact) (models.Contact, error) {
		called = true
		return ct, nil
	}
	req2 := httptest.NewRequest("POST", "/v1/contacts",
		strings.NewReader(`{"name":"Bob","emails":["bob@x.com"]}`))
	req2.Header.Set("Content-Type", "application/json")
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
	if called {
		t.Fatalf("PUT seam invoked despite missing CardDAV URL")
	}
}

// Updating an event must route through the brokered CalDAV client's UpdateEvent
// and return {updated:true}.
func TestBrokeredUpdateEvent(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	fake := &fakeCalDAV{}
	withStubbedCalDAVDial(t, fake, nil)

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	req := httptest.NewRequest("PUT", "/v1/calendar/events/evt-1",
		strings.NewReader(`{"summary":"Renamed","start":"2026-06-26T10:00:00Z","end":"2026-06-26T11:00:00Z","path":"/cal/evt-1.ics"}`))
	req.Header.Set("Content-Type", "application/json")
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
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Updated bool `json:"updated"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !out.Updated {
		t.Fatalf("want {updated:true}, got: %s", body)
	}
}
