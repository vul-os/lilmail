package api

import (
	"strings"
	"testing"

	"lilmail/models"

	vcard "github.com/emersion/go-vcard"
)

// The rich round-trip must preserve every extended field through a vCard 4.0
// encode/decode: structured N, TYPE labels on EMAIL/TEL, ADR, BDAY/ANNIVERSARY,
// URL, IMPP, ORG department (component 2), nickname and CATEGORIES.
func TestRichContactRoundTrip(t *testing.T) {
	in := models.Contact{
		UID:  "u-rich",
		Name: "Dr. Ada K. Lovelace",
		StructuredName: &models.StructuredName{
			Prefix: "Dr.", First: "Ada", Middle: "King", Last: "Lovelace", Suffix: "Esq.",
		},
		Nickname:    "Countess",
		Org:         "Analytical Engines",
		Department:  "Research",
		Title:       "Mathematician",
		Note:        "first programmer",
		Birthday:    "1815-12-10",
		Anniversary: "1835-07-08",
		TypedEmails: []models.TypedValue{
			{Value: "ada@work.example", Type: "work"},
			{Value: "ada@home.example", Type: "home"},
		},
		TypedPhones: []models.TypedValue{
			{Value: "+1 555 0100", Type: "mobile"},
			{Value: "+1 555 0200", Type: "work"},
		},
		Websites: []models.TypedValue{{Value: "https://ada.example", Type: "work"}},
		IMs:      []models.TypedValue{{Value: "xmpp:ada@im.example", Type: "home"}},
		Addresses: []models.Address{{
			Type: "home", Street: "1 Engine Way", Locality: "London",
			Region: "England", Postal: "SW1", Country: "UK",
		}},
		Groups: []string{"Friends", "VIP"},
	}

	// Encode then re-decode via a real vCard byte round-trip (not just in-memory
	// Card reuse) so the TYPE params + structured values survive serialisation.
	card := cardFromContact(in)
	var buf strings.Builder
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := vcard.NewDecoder(strings.NewReader(buf.String()))
	back, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := contactFromCard(back, "/books/u-rich.vcf")

	if out.StructuredName == nil {
		t.Fatal("structured name lost")
	}
	sn := out.StructuredName
	if sn.Prefix != "Dr." || sn.First != "Ada" || sn.Middle != "King" || sn.Last != "Lovelace" || sn.Suffix != "Esq." {
		t.Errorf("N mismatch: %+v", sn)
	}
	if out.Nickname != "Countess" {
		t.Errorf("nickname: %q", out.Nickname)
	}
	if out.Org != "Analytical Engines" || out.Department != "Research" {
		t.Errorf("ORG/dept: %q / %q", out.Org, out.Department)
	}
	if out.Birthday != "1815-12-10" || out.Anniversary != "1835-07-08" {
		t.Errorf("BDAY/ANNIVERSARY: %q / %q", out.Birthday, out.Anniversary)
	}
	if len(out.TypedEmails) != 2 || out.TypedEmails[0].Type != "work" || out.TypedEmails[1].Type != "home" {
		t.Errorf("EMAIL TYPE labels: %+v", out.TypedEmails)
	}
	// mobile -> cell (canonical) -> mobile round-trip.
	if len(out.TypedPhones) != 2 || out.TypedPhones[0].Type != "mobile" || out.TypedPhones[1].Type != "work" {
		t.Errorf("TEL TYPE labels: %+v", out.TypedPhones)
	}
	if len(out.Websites) != 1 || out.Websites[0].Value != "https://ada.example" || out.Websites[0].Type != "work" {
		t.Errorf("URL: %+v", out.Websites)
	}
	if len(out.IMs) != 1 || out.IMs[0].Value != "xmpp:ada@im.example" {
		t.Errorf("IMPP: %+v", out.IMs)
	}
	if len(out.Addresses) != 1 {
		t.Fatalf("ADR count: %d", len(out.Addresses))
	}
	a := out.Addresses[0]
	if a.Street != "1 Engine Way" || a.Locality != "London" || a.Region != "England" || a.Postal != "SW1" || a.Country != "UK" || a.Type != "home" {
		t.Errorf("ADR mismatch: %+v", a)
	}
	if len(out.Groups) != 2 {
		t.Errorf("CATEGORIES: %+v", out.Groups)
	}
}

// A card that supplies only the flat Emails/Phones projection (legacy client)
// still round-trips, and the flat slice is repopulated on read.
func TestFlatProjectionStillWorks(t *testing.T) {
	in := models.Contact{Name: "Flat User", Emails: []string{"flat@x.com"}, Phones: []string{"+1 555"}}
	out := contactFromCard(cardFromContact(in), "")
	if len(out.Emails) != 1 || out.Emails[0] != "flat@x.com" {
		t.Errorf("flat emails: %+v", out.Emails)
	}
	if len(out.Phones) != 1 || out.Phones[0] != "+1 555" {
		t.Errorf("flat phones: %+v", out.Phones)
	}
}

// A custom (non-well-known) TYPE label is preserved verbatim.
func TestCustomTypeLabelPreserved(t *testing.T) {
	in := models.Contact{Name: "C", TypedEmails: []models.TypedValue{{Value: "c@x.com", Type: "school"}}}
	out := contactFromCard(cardFromContact(in), "")
	if len(out.TypedEmails) != 1 || out.TypedEmails[0].Type != "school" {
		t.Errorf("custom type lost: %+v", out.TypedEmails)
	}
}

// Exported CardFromContact/ContactFromCard wrappers behave like the internal ones.
func TestExportedWrappers(t *testing.T) {
	in := models.Contact{Name: "Wrapped", Emails: []string{"w@x.com"}}
	out := ContactFromCard(CardFromContact(in))
	if out.Name != "Wrapped" || len(out.Emails) != 1 {
		t.Errorf("wrapper round-trip: %+v", out)
	}
}
