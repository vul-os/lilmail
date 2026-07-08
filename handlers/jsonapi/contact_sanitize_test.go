package jsonapi

import (
	"strings"
	"testing"

	"lilmail/models"
)

// Control characters (the vector for vCard line/property injection) must be
// stripped from every scalar field before the card is written.
func TestSanitizeStripsControlChars(t *testing.T) {
	ct := sanitizeContact(models.Contact{
		Name:  "Ada\r\nEMAIL:injected@evil.com",
		Note:  "line1\nline2",
		Emails: []string{"ok@x.com\r\nFN:evil"},
	})
	if strings.ContainsAny(ct.Name, "\r\n") {
		t.Errorf("name still has CR/LF: %q", ct.Name)
	}
	// A newline in a value is folded to a space, not kept as a line break.
	for _, e := range ct.Emails {
		if strings.ContainsAny(e, "\r\n") {
			t.Errorf("email still has CR/LF: %q", e)
		}
	}
}

// Oversized fields are truncated to their caps rather than rejected.
func TestSanitizeClampsLength(t *testing.T) {
	long := strings.Repeat("a", maxFieldLen+500)
	ct := sanitizeContact(models.Contact{Name: long, Emails: []string{"x@y.com"}})
	if len(ct.Name) > maxFieldLen {
		t.Errorf("name not clamped: %d", len(ct.Name))
	}
}

// The number of list items per collection is capped.
func TestSanitizeCapsListItems(t *testing.T) {
	emails := make([]string, maxListItems+50)
	for i := range emails {
		emails[i] = "user@x.com"
	}
	ct := sanitizeContact(models.Contact{Name: "N", Emails: emails})
	if len(ct.Emails) > maxListItems {
		t.Errorf("emails not capped: %d", len(ct.Emails))
	}
}

// Empty rows in typed collections are dropped.
func TestSanitizeDropsEmptyTyped(t *testing.T) {
	ct := sanitizeContact(models.Contact{
		Name:        "N",
		TypedEmails: []models.TypedValue{{Value: "  "}, {Value: "real@x.com", Type: "WORK"}},
	})
	if len(ct.TypedEmails) != 1 || ct.TypedEmails[0].Value != "real@x.com" {
		t.Fatalf("empty typed not dropped: %+v", ct.TypedEmails)
	}
	if ct.TypedEmails[0].Type != "work" {
		t.Errorf("type not lowercased: %q", ct.TypedEmails[0].Type)
	}
}

// hasIdentity accepts a structured-name-only contact and rejects an empty one.
func TestHasIdentity(t *testing.T) {
	if hasIdentity(models.Contact{}) {
		t.Error("empty contact should have no identity")
	}
	if !hasIdentity(models.Contact{StructuredName: &models.StructuredName{First: "Ada"}}) {
		t.Error("structured-name contact should have identity")
	}
	if !hasIdentity(models.Contact{TypedEmails: []models.TypedValue{{Value: "a@x.com"}}}) {
		t.Error("typed-email contact should have identity")
	}
}
