// handlers/api/contact_vcard.go — rich vCard 4.0 <-> models.Contact mapping.
//
// This is the round-trip that gives Vulos Contacts its Google-Contacts field
// depth. Each model field maps to the correct vCard 4.0 property so a card
// authored here syncs cleanly to any CardDAV client and back:
//
//	StructuredName  -> N            (family;given;additional;prefix;suffix)
//	Nickname        -> NICKNAME
//	FileAs          -> SORT-AS param on N (X-ABShowAs mirror kept off — SORT-AS is standard)
//	Org + Department-> ORG          (org;dept  — component 2 is the department)
//	Title           -> TITLE
//	Note            -> NOTE
//	TypedEmails     -> EMAIL;TYPE=
//	TypedPhones     -> TEL;TYPE=
//	Addresses       -> ADR;TYPE=    (pobox;ext;street;locality;region;postal;country)
//	Websites        -> URL;TYPE=
//	IMs             -> IMPP;TYPE=
//	Birthday        -> BDAY
//	Anniversary     -> ANNIVERSARY
//	Groups          -> CATEGORIES
//
// Backwards compatibility: the flat Emails/Phones []string projection is always
// populated on read (from the typed slices) and accepted on write (used when no
// typed slice is supplied), so existing callers and the lean autocomplete are
// untouched.
package api

import (
	"strings"

	"lilmail/models"

	vcard "github.com/emersion/go-vcard"
)

// CardFromContact / ContactFromCard are exported so the jsonapi import/export
// surface can turn contacts into vCard bytes and back without re-implementing the
// mapping. They wrap the internal round-trip used by the CardDAV client.
func CardFromContact(ct models.Contact) vcard.Card    { return cardFromContact(ct) }
func ContactFromCard(card vcard.Card) models.Contact { return contactFromCard(card, "") }

// knownTypes are the well-known lowercase TYPE tokens the UI offers. Any other
// (non-empty) token is preserved verbatim as a custom label.
var knownEmailPhoneTypes = map[string]bool{
	"home": true, "work": true, "mobile": true, "other": true,
}

// normType lowercases and trims a type label. Empty stays empty (unlabelled).
func normType(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// typeParams builds a Params carrying a single TYPE= when t is non-empty. vCard
// TYPE values are case-insensitive; we emit lowercase. "mobile" is mapped to the
// vCard-canonical "cell" for TEL so phones interoperate with strict clients,
// while the reverse map restores "mobile" on read.
func typeParams(field, t string) vcard.Params {
	t = normType(t)
	if t == "" {
		return nil
	}
	if field == vcard.FieldTelephone && t == "mobile" {
		t = vcard.TypeCell
	}
	p := make(vcard.Params)
	p.Add(vcard.ParamType, t)
	return p
}

// firstType returns the first TYPE parameter of a field, lowercased, mapping the
// vCard TEL "cell" back to the UI's "mobile". Empty when unlabelled.
func firstType(field string, f *vcard.Field) string {
	if f == nil || f.Params == nil {
		return ""
	}
	for _, t := range f.Params.Types() {
		t = normType(t)
		if t == "" || t == "pref" || t == "internet" || t == "voice" {
			continue
		}
		if field == vcard.FieldTelephone && t == vcard.TypeCell {
			return "mobile"
		}
		return t
	}
	return ""
}

// contactFromCard decodes a vCard into the rich model. It never errors: unknown
// or malformed components degrade to empty rather than dropping the whole card.
func contactFromCard(card vcard.Card, objPath string) models.Contact {
	ct := models.Contact{UID: cardUID(card), Path: objPath}

	if f := card.Get(vcard.FieldFormattedName); f != nil {
		ct.Name = f.Value
	}
	if n := card.Name(); n != nil {
		sn := &models.StructuredName{
			Prefix: n.HonorificPrefix,
			First:  n.GivenName,
			Middle: n.AdditionalName,
			Last:   n.FamilyName,
			Suffix: n.HonorificSuffix,
		}
		if *sn != (models.StructuredName{}) {
			ct.StructuredName = sn
		}
	}
	if f := card.Get(vcard.FieldName); f != nil && f.Params != nil {
		ct.FileAs = f.Params.Get(vcard.ParamSortAs)
	}
	if f := card.Get(vcard.FieldNickname); f != nil {
		ct.Nickname = f.Value
	}
	if f := card.Get(vcard.FieldOrganization); f != nil {
		parts := strings.Split(f.Value, ";")
		ct.Org = strings.TrimSpace(parts[0])
		if len(parts) > 1 {
			ct.Department = strings.TrimSpace(parts[1])
		}
	}
	if f := card.Get(vcard.FieldTitle); f != nil {
		ct.Title = f.Value
	}
	if f := card.Get(vcard.FieldNote); f != nil {
		ct.Note = f.Value
	}
	if f := card.Get(vcard.FieldBirthday); f != nil {
		ct.Birthday = f.Value
	}
	if f := card.Get(vcard.FieldAnniversary); f != nil {
		ct.Anniversary = f.Value
	}

	for _, f := range card[vcard.FieldEmail] {
		if v := strings.TrimSpace(f.Value); v != "" {
			ct.TypedEmails = append(ct.TypedEmails, models.TypedValue{Value: v, Type: firstType(vcard.FieldEmail, f)})
			ct.Emails = append(ct.Emails, v)
		}
	}
	for _, f := range card[vcard.FieldTelephone] {
		if v := strings.TrimSpace(f.Value); v != "" {
			ct.TypedPhones = append(ct.TypedPhones, models.TypedValue{Value: v, Type: firstType(vcard.FieldTelephone, f)})
			ct.Phones = append(ct.Phones, v)
		}
	}
	for _, f := range card[vcard.FieldURL] {
		if v := strings.TrimSpace(f.Value); v != "" {
			ct.Websites = append(ct.Websites, models.TypedValue{Value: v, Type: firstType(vcard.FieldURL, f)})
		}
	}
	for _, f := range card[vcard.FieldIMPP] {
		if v := strings.TrimSpace(f.Value); v != "" {
			ct.IMs = append(ct.IMs, models.TypedValue{Value: v, Type: firstType(vcard.FieldIMPP, f)})
		}
	}
	for _, a := range card.Addresses() {
		adr := models.Address{
			Type:     firstType(vcard.FieldAddress, a.Field),
			POBox:    a.PostOfficeBox,
			Extended: a.ExtendedAddress,
			Street:   a.StreetAddress,
			Locality: a.Locality,
			Region:   a.Region,
			Postal:   a.PostalCode,
			Country:  a.Country,
		}
		if !adr.IsEmpty() {
			ct.Addresses = append(ct.Addresses, adr)
		}
	}
	if cats := card.Categories(); len(cats) > 0 {
		for _, c := range cats {
			if c = strings.TrimSpace(c); c != "" {
				ct.Groups = append(ct.Groups, c)
			}
		}
	}

	if ct.Emails == nil {
		ct.Emails = []string{}
	}
	return ct
}

// cardFromContact encodes the rich model into a vCard 4.0. When a typed slice is
// present it is authoritative; otherwise the flat Emails/Phones projection is
// used, so a legacy client that only sends emails[] still round-trips.
func cardFromContact(ct models.Contact) vcard.Card {
	card := make(vcard.Card)
	card.SetValue(vcard.FieldUID, ct.UID)

	// FN — prefer the explicit name, then a derived one from N, then first email.
	name := strings.TrimSpace(ct.Name)
	if name == "" && ct.StructuredName != nil {
		name = joinName(ct.StructuredName)
	}
	if name == "" && len(effectiveEmails(ct)) > 0 {
		name = effectiveEmails(ct)[0]
	}
	card.SetValue(vcard.FieldFormattedName, name)

	// N — a structured N is required by vCard 3.0 and expected by every client.
	n := &vcard.Name{}
	if ct.StructuredName != nil {
		n.HonorificPrefix = ct.StructuredName.Prefix
		n.GivenName = ct.StructuredName.First
		n.AdditionalName = ct.StructuredName.Middle
		n.FamilyName = ct.StructuredName.Last
		n.HonorificSuffix = ct.StructuredName.Suffix
	} else {
		// Best-effort split of a free-form name: "First Last" -> given+family.
		n.GivenName, n.FamilyName = splitDisplayName(name)
	}
	card.SetName(n)
	if fa := strings.TrimSpace(ct.FileAs); fa != "" {
		if f := card.Get(vcard.FieldName); f != nil {
			if f.Params == nil {
				f.Params = make(vcard.Params)
			}
			f.Params.Set(vcard.ParamSortAs, fa)
		}
	}

	if nn := strings.TrimSpace(ct.Nickname); nn != "" {
		card.SetValue(vcard.FieldNickname, nn)
	}
	// ORG — component 1 is the org, component 2 the department.
	org := strings.TrimSpace(ct.Org)
	dept := strings.TrimSpace(ct.Department)
	if org != "" || dept != "" {
		val := org
		if dept != "" {
			val = org + ";" + dept
		}
		card.SetValue(vcard.FieldOrganization, val)
	}
	if t := strings.TrimSpace(ct.Title); t != "" {
		card.SetValue(vcard.FieldTitle, t)
	}
	if ct.Note != "" {
		card.SetValue(vcard.FieldNote, ct.Note)
	}
	if b := strings.TrimSpace(ct.Birthday); b != "" {
		card.SetValue(vcard.FieldBirthday, b)
	}
	if a := strings.TrimSpace(ct.Anniversary); a != "" {
		card.SetValue(vcard.FieldAnniversary, a)
	}

	for _, tv := range typedOrFlat(ct.TypedEmails, ct.Emails) {
		if v := strings.TrimSpace(tv.Value); v != "" {
			card.Add(vcard.FieldEmail, &vcard.Field{Value: v, Params: typeParams(vcard.FieldEmail, tv.Type)})
		}
	}
	for _, tv := range typedOrFlat(ct.TypedPhones, ct.Phones) {
		if v := strings.TrimSpace(tv.Value); v != "" {
			card.Add(vcard.FieldTelephone, &vcard.Field{Value: v, Params: typeParams(vcard.FieldTelephone, tv.Type)})
		}
	}
	for _, tv := range ct.Websites {
		if v := strings.TrimSpace(tv.Value); v != "" {
			card.Add(vcard.FieldURL, &vcard.Field{Value: v, Params: typeParams(vcard.FieldURL, tv.Type)})
		}
	}
	for _, tv := range ct.IMs {
		if v := strings.TrimSpace(tv.Value); v != "" {
			card.Add(vcard.FieldIMPP, &vcard.Field{Value: v, Params: typeParams(vcard.FieldIMPP, tv.Type)})
		}
	}
	for _, adr := range ct.Addresses {
		if adr.IsEmpty() {
			continue
		}
		// ADR is a 7-component structured value: pobox;ext;street;locality;
		// region;postal;country. go-vcard's Address.field() is unexported, so we
		// build the Field directly to attach a TYPE param.
		val := strings.Join([]string{
			adr.POBox, adr.Extended, adr.Street, adr.Locality,
			adr.Region, adr.Postal, adr.Country,
		}, ";")
		card.Add(vcard.FieldAddress, &vcard.Field{
			Value:  val,
			Params: typeParams(vcard.FieldAddress, adr.Type),
		})
	}
	if cats := dedupeNonEmpty(ct.Groups); len(cats) > 0 {
		card.SetCategories(cats)
	}

	// Normalise to vCard 4.0 (sets VERSION) so servers accept the PUT.
	vcard.ToV4(card)
	return card
}

// effectiveEmails returns the address list regardless of typed/flat form.
func effectiveEmails(ct models.Contact) []string {
	if len(ct.TypedEmails) > 0 {
		out := make([]string, 0, len(ct.TypedEmails))
		for _, tv := range ct.TypedEmails {
			if v := strings.TrimSpace(tv.Value); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return ct.Emails
}

// typedOrFlat returns typed when non-empty, else adapts the flat []string.
func typedOrFlat(typed []models.TypedValue, flat []string) []models.TypedValue {
	if len(typed) > 0 {
		return typed
	}
	out := make([]models.TypedValue, 0, len(flat))
	for _, v := range flat {
		out = append(out, models.TypedValue{Value: v})
	}
	return out
}

func joinName(n *models.StructuredName) string {
	parts := []string{n.Prefix, n.First, n.Middle, n.Last, n.Suffix}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// splitDisplayName makes a best-effort given/family split from a display name.
// "Ada Lovelace" -> ("Ada", "Lovelace"); a single token becomes the given name.
func splitDisplayName(name string) (given, family string) {
	fields := strings.Fields(name)
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], strings.Join(fields[1:], " ")
	}
}

func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	return out
}
