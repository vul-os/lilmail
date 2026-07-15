package jsonapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// brokeredContactReq builds a brokered contacts request with the CardDAV URL set.
func brokeredContactReq(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		r.Header.Set(k, v)
	}
	r.Header.Set(hdrMailCardDAVURL, "https://dav.example.com/carddav/")
	return r
}

// Updating a contact (PUT /v1/contacts/:uid) must route through the bearer PUT
// seam, force the UID from the path (a client cannot retarget another card via the
// body), and return the saved card.
func TestBrokeredUpdateContact(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var got models.Contact
	orig := brokerContactPut
	brokerContactPut = func(_ brokerSpec, ct models.Contact) (models.Contact, error) {
		got = ct
		return ct, nil
	}
	t.Cleanup(func() { brokerContactPut = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// Body carries a DIFFERENT uid — the path uid must win.
	body := `{"uid":"attacker-uid","name":"Carol","emails":["carol@x.com"]}`
	resp, err := app.Test(brokeredContactReq("PUT", "/v1/contacts/real-uid", body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	if got.UID != "real-uid" {
		t.Fatalf("path uid must override the body uid: got %q", got.UID)
	}
	if got.Name != "Carol" || len(got.Emails) != 1 || got.Emails[0] != "carol@x.com" {
		t.Fatalf("contact not threaded to seam: %+v", got)
	}
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Contact models.Contact `json:"contact"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Contact.UID != "real-uid" {
		t.Fatalf("saved card not returned: %s", b)
	}
}

// A create/update with neither a name nor an email must fail closed with 400 and
// never reach the CardDAV seam (no empty ghost cards).
func TestContactCreateRequiresIdentity(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	called := false
	orig := brokerContactPut
	brokerContactPut = func(_ brokerSpec, ct models.Contact) (models.Contact, error) {
		called = true
		return ct, nil
	}
	t.Cleanup(func() { brokerContactPut = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	for _, target := range []string{"/v1/contacts", "/v1/contacts/some-uid"} {
		method := "POST"
		if strings.Contains(target, "some-uid") {
			method = "PUT"
		}
		resp, err := app.Test(brokeredContactReq(method, target, `{"phones":["+123"]}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s %s: want 400 (needs name/email), got %d", method, target, resp.StatusCode)
		}
	}
	if called {
		t.Fatalf("CardDAV PUT seam invoked for an identity-less contact")
	}
}

// Deleting a contact (DELETE /v1/contacts/:uid?path=) must route through the
// bearer delete seam with both the uid and the object path, and return 204.
func TestBrokeredDeleteContact(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	var gotUID, gotPath string
	orig := brokerContactDelete
	brokerContactDelete = func(_ brokerSpec, uid, objPath string) error {
		gotUID, gotPath = uid, objPath
		return nil
	}
	t.Cleanup(func() { brokerContactDelete = orig })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	resp, err := app.Test(brokeredContactReq("DELETE", "/v1/contacts/u9?path=/ab/u9.vcf", ""))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if gotUID != "u9" || gotPath != "/ab/u9.vcf" {
		t.Fatalf("delete not threaded to seam: uid=%q path=%q", gotUID, gotPath)
	}
}

// Contacts CRUD on a brokered account WITHOUT a CardDAV URL must all report 501
// and never invoke any CardDAV seam (no session fallthrough).
func TestContactsCRUDMissingURLNotImplemented(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")

	putCalled, delCalled := false, false
	origPut, origDel := brokerContactPut, brokerContactDelete
	brokerContactPut = func(_ brokerSpec, ct models.Contact) (models.Contact, error) {
		putCalled = true
		return ct, nil
	}
	brokerContactDelete = func(brokerSpec, string, string) error {
		delCalled = true
		return nil
	}
	t.Cleanup(func() { brokerContactPut = origPut; brokerContactDelete = origDel })

	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)

	// Requests WITHOUT the CardDAV URL header.
	noURL := func(method, target, body string) *http.Request {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, target, nil)
		} else {
			r = httptest.NewRequest(method, target, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set(hdrBrokerAuth, "s3cr3t")
		for k, v := range brokeredHeaders() {
			r.Header.Set(k, v)
		}
		return r
	}

	cases := []*http.Request{
		noURL("POST", "/v1/contacts", `{"name":"X"}`),
		noURL("PUT", "/v1/contacts/u1", `{"name":"X"}`),
		noURL("DELETE", "/v1/contacts/u1", ""),
	}
	for _, req := range cases {
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request %s %s: %v", req.Method, req.URL.Path, err)
		}
		if resp.StatusCode != fiber.StatusNotImplemented {
			t.Fatalf("%s %s: want 501, got %d", req.Method, req.URL.Path, resp.StatusCode)
		}
	}
	if putCalled || delCalled {
		t.Fatalf("a CardDAV seam was invoked despite a missing CardDAV URL")
	}
}
