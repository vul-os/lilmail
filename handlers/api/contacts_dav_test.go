package api

import (
	"testing"

	"lilmail/models"
)

// The vCard round-trip must preserve the fields the contacts view depends on:
// name, org, multiple emails/phones, and the UID identity.
func TestContactCardRoundTrip(t *testing.T) {
	in := models.Contact{
		UID:    "u-123",
		Name:   "Alice Mokoena",
		Org:    "Vulos",
		Title:  "Engineer",
		Note:   "met at conf",
		Emails: []string{"alice@vulos.org", "alice@personal.io"},
		Phones: []string{"+27 11 555 0000"},
	}
	card := cardFromContact(in)
	out := contactFromCard(card, "/books/default/u-123.vcf")

	if out.UID != in.UID {
		t.Errorf("UID: got %q want %q", out.UID, in.UID)
	}
	if out.Name != in.Name {
		t.Errorf("Name: got %q want %q", out.Name, in.Name)
	}
	if out.Org != in.Org {
		t.Errorf("Org: got %q want %q", out.Org, in.Org)
	}
	if out.Title != in.Title {
		t.Errorf("Title: got %q want %q", out.Title, in.Title)
	}
	if len(out.Emails) != 2 || out.Emails[0] != "alice@vulos.org" {
		t.Errorf("Emails: got %v", out.Emails)
	}
	if len(out.Phones) != 1 || out.Phones[0] != "+27 11 555 0000" {
		t.Errorf("Phones: got %v", out.Phones)
	}
	if out.Path != "/books/default/u-123.vcf" {
		t.Errorf("Path: got %q", out.Path)
	}
}

// A contact with no name falls back to its primary email as the display name so
// the card is never anonymous in the list.
func TestContactNameFallback(t *testing.T) {
	card := cardFromContact(models.Contact{Emails: []string{"noname@x.com"}})
	out := contactFromCard(card, "")
	if out.Name != "noname@x.com" {
		t.Errorf("expected email fallback name, got %q", out.Name)
	}
}

func TestContactMatches(t *testing.T) {
	ct := models.Contact{Name: "Bob Osei", Org: "DesignCo", Emails: []string{"bob@designco.io"}}
	for _, q := range []string{"bob", "osei", "designco", "designco.io"} {
		if !contactMatches(ct, q) {
			t.Errorf("expected match for %q", q)
		}
	}
	if contactMatches(ct, "zzz") {
		t.Error("unexpected match for zzz")
	}
}
