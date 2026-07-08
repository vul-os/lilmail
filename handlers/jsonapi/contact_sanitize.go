// handlers/jsonapi/contact_sanitize.go — bounds + normalisation for untrusted
// contact input (create/update bodies AND import rows).
//
// SECURITY: every contact that reaches the CardDAV write path passes through
// sanitizeContact first, so a single caller cannot author an unbounded card that
// bloats the address book or smuggles control characters / header-injection
// sequences into a vCard property. Caps are generous for real contacts but hard.
package jsonapi

import (
	"strings"

	"lilmail/models"
)

const (
	maxFieldLen    = 1024 // per scalar field (name, org, note is larger below)
	maxNoteLen     = 8192
	maxValueLen    = 512  // per email/phone/url/im value
	maxListItems   = 64   // per typed collection (emails, phones, addresses, ...)
	maxGroups      = 128  // categories on one card
	maxGroupLen    = 128
	maxTypeLen     = 64
)

// sanitizeContact clamps every field of a contact to safe bounds and strips
// control characters (which have no place in a vCard property and are the vector
// for property/line injection). It is idempotent and never errors — an
// over-long or malformed field is truncated/dropped, not rejected, so bulk
// import stays best-effort.
func sanitizeContact(ct models.Contact) models.Contact {
	ct.Name = clampField(ct.Name, maxFieldLen)
	ct.Nickname = clampField(ct.Nickname, maxFieldLen)
	ct.FileAs = clampField(ct.FileAs, maxFieldLen)
	ct.Org = clampField(ct.Org, maxFieldLen)
	ct.Department = clampField(ct.Department, maxFieldLen)
	ct.Title = clampField(ct.Title, maxFieldLen)
	ct.Note = clampField(ct.Note, maxNoteLen)
	ct.Birthday = clampField(ct.Birthday, maxTypeLen)
	ct.Anniversary = clampField(ct.Anniversary, maxTypeLen)

	if ct.StructuredName != nil {
		ct.StructuredName.Prefix = clampField(ct.StructuredName.Prefix, maxFieldLen)
		ct.StructuredName.First = clampField(ct.StructuredName.First, maxFieldLen)
		ct.StructuredName.Middle = clampField(ct.StructuredName.Middle, maxFieldLen)
		ct.StructuredName.Last = clampField(ct.StructuredName.Last, maxFieldLen)
		ct.StructuredName.Suffix = clampField(ct.StructuredName.Suffix, maxFieldLen)
		if *ct.StructuredName == (models.StructuredName{}) {
			ct.StructuredName = nil
		}
	}

	ct.Emails = clampStrings(ct.Emails, maxValueLen)
	ct.Phones = clampStrings(ct.Phones, maxValueLen)
	ct.TypedEmails = clampTyped(ct.TypedEmails)
	ct.TypedPhones = clampTyped(ct.TypedPhones)
	ct.Websites = clampTyped(ct.Websites)
	ct.IMs = clampTyped(ct.IMs)
	ct.Groups = clampGroups(ct.Groups)
	ct.Addresses = clampAddresses(ct.Addresses)
	return ct
}

// hasIdentity reports whether the contact carries at least a name or one email,
// counting both the flat and typed email projections.
func hasIdentity(ct models.Contact) bool {
	if strings.TrimSpace(ct.Name) != "" {
		return true
	}
	if ct.StructuredName != nil && *ct.StructuredName != (models.StructuredName{}) {
		return true
	}
	if len(ct.Emails) > 0 {
		return true
	}
	for _, e := range ct.TypedEmails {
		if strings.TrimSpace(e.Value) != "" {
			return true
		}
	}
	return false
}

// stripContactControl removes CR/LF and other control runes (except tab) that could
// break a vCard line or inject a property, and trims surrounding whitespace.
func stripContactControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			b.WriteByte(' ')
			continue
		}
		if r < 0x20 && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func clampField(s string, max int) string {
	s = stripContactControl(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func clampStrings(in []string, max int) []string {
	if in == nil {
		return in
	}
	if len(in) > maxListItems {
		in = in[:maxListItems]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = clampField(s, max); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func clampTyped(in []models.TypedValue) []models.TypedValue {
	if in == nil {
		return in
	}
	if len(in) > maxListItems {
		in = in[:maxListItems]
	}
	out := make([]models.TypedValue, 0, len(in))
	for _, tv := range in {
		v := clampField(tv.Value, maxValueLen)
		if v == "" {
			continue
		}
		out = append(out, models.TypedValue{
			Value: v,
			Type:  strings.ToLower(clampField(tv.Type, maxTypeLen)),
		})
	}
	return out
}

func clampGroups(in []string) []string {
	if in == nil {
		return in
	}
	if len(in) > maxGroups {
		in = in[:maxGroups]
	}
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g = clampField(g, maxGroupLen); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func clampAddresses(in []models.Address) []models.Address {
	if in == nil {
		return in
	}
	if len(in) > maxListItems {
		in = in[:maxListItems]
	}
	out := make([]models.Address, 0, len(in))
	for _, a := range in {
		a.Type = strings.ToLower(clampField(a.Type, maxTypeLen))
		a.POBox = clampField(a.POBox, maxFieldLen)
		a.Extended = clampField(a.Extended, maxFieldLen)
		a.Street = clampField(a.Street, maxFieldLen)
		a.Locality = clampField(a.Locality, maxFieldLen)
		a.Region = clampField(a.Region, maxFieldLen)
		a.Postal = clampField(a.Postal, maxFieldLen)
		a.Country = clampField(a.Country, maxFieldLen)
		if !a.IsEmpty() {
			out = append(out, a)
		}
	}
	return out
}
