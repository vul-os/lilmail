package models

// Contact is a full address-book entry parsed from a CardDAV vCard. Unlike the
// lean RecipientEntry (email + name) used by compose autocomplete, this carries
// the fields a real contacts view needs to list, edit and round-trip a card.
//
// UID is the vCard UID (stable identity); Path is the CardDAV object path the
// card lives at (server-assigned, empty for a not-yet-saved contact). Emails and
// Phones preserve order; the first of each is treated as primary by the UI.
type Contact struct {
	UID    string   `json:"uid"`
	Name   string   `json:"name"`
	Org    string   `json:"org,omitempty"`
	Title  string   `json:"title,omitempty"`
	Note   string   `json:"note,omitempty"`
	Emails []string `json:"emails"`
	Phones []string `json:"phones,omitempty"`
	Path   string   `json:"path,omitempty"`
}
