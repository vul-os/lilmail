package models

// Contact is a full address-book entry parsed from a CardDAV vCard. Unlike the
// lean RecipientEntry (email + name) used by compose autocomplete, this carries
// the fields a real contacts view needs to list, edit and round-trip a card.
//
// UID is the vCard UID (stable identity); Path is the CardDAV object path the
// card lives at (server-assigned, empty for a not-yet-saved contact). Emails and
// Phones preserve order; the first of each is treated as primary by the UI.
//
// The typed collections (Emails/Phones/Addresses/Websites/IMs) carry an optional
// Type label (home/work/mobile/other or a custom string) so the card round-trips
// through CardDAV with TYPE parameters intact. The scalar Name/Org/Title fields
// are kept for backwards compatibility with the lean projection; the structured
// StructuredName + Nickname + FileAs + Department + Birthday + Anniversary extend
// the card to Google-Contacts parity.
type Contact struct {
	UID  string `json:"uid"`
	Name string `json:"name"`

	// StructuredName is the vCard N property (prefix/first/middle/last/suffix).
	// Optional: when absent the round-trip derives a best-effort N from Name.
	StructuredName *StructuredName `json:"structuredName,omitempty"`
	Nickname       string          `json:"nickname,omitempty"`
	// FileAs is the SORT-AS / X-ABShowAs hint for list ordering ("Last, First").
	FileAs string `json:"fileAs,omitempty"`

	Org        string `json:"org,omitempty"`
	Department string `json:"department,omitempty"` // vCard ORG component 2
	Title      string `json:"title,omitempty"`
	Note       string `json:"note,omitempty"`

	// Emails/Phones remain []string for the primary (unlabelled) projection so
	// existing callers and the lean autocomplete keep working. TypedEmails /
	// TypedPhones carry the label metadata; when present they are authoritative
	// and Emails/Phones are derived from them for compatibility.
	Emails []string `json:"emails"`
	Phones []string `json:"phones,omitempty"`

	TypedEmails []TypedValue `json:"typedEmails,omitempty"`
	TypedPhones []TypedValue `json:"typedPhones,omitempty"`
	Addresses   []Address    `json:"addresses,omitempty"`
	Websites    []TypedValue `json:"websites,omitempty"`
	IMs         []TypedValue `json:"ims,omitempty"`
	Birthday    string       `json:"birthday,omitempty"`    // ISO date (YYYY-MM-DD) or vCard raw
	Anniversary string       `json:"anniversary,omitempty"` // ISO date or vCard raw
	Groups      []string     `json:"groups,omitempty"`      // CATEGORIES membership

	// Photo is the contact avatar as a data URI ("data:image/png;base64,...").
	// It maps to the vCard PHOTO property. Only a raster image (PNG/JPEG/GIF/WebP)
	// is ever accepted or emitted: the write path content-sniffs and size-caps the
	// bytes so no SVG/HTML (a stored-XSS vector) and no unbounded blob can enter a
	// card. Empty when the contact has no photo (the UI falls back to initials).
	Photo string `json:"photo,omitempty"`

	// Starred marks a favourite contact. It is modelled as a reserved CATEGORIES
	// membership (starredCategory) so it round-trips through CardDAV like any other
	// group, but it is surfaced as a boolean here and hidden from the group list.
	Starred bool `json:"starred,omitempty"`

	Path string `json:"path,omitempty"`
}

// StructuredName is the vCard N property, split into its five components.
type StructuredName struct {
	Prefix string `json:"prefix,omitempty"`
	First  string `json:"first,omitempty"`
	Middle string `json:"middle,omitempty"`
	Last   string `json:"last,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}

// TypedValue is a single value (email address, phone number, URL, IM handle)
// with an optional type label. Type is a lowercase token: one of the well-known
// labels (home/work/mobile/other) or a custom string; empty means unlabelled.
type TypedValue struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// Address is a structured postal address (vCard ADR) with an optional type.
type Address struct {
	Type     string `json:"type,omitempty"`
	POBox    string `json:"poBox,omitempty"`
	Extended string `json:"extended,omitempty"`
	Street   string `json:"street,omitempty"`
	Locality string `json:"locality,omitempty"` // city
	Region   string `json:"region,omitempty"`   // state / province
	Postal   string `json:"postal,omitempty"`   // postal / zip code
	Country  string `json:"country,omitempty"`
}

// IsEmpty reports whether an Address carries no address data (only used to skip
// blank ADR rows on round-trip so an empty row never produces an empty vCard
// property).
func (a Address) IsEmpty() bool {
	return a.POBox == "" && a.Extended == "" && a.Street == "" &&
		a.Locality == "" && a.Region == "" && a.Postal == "" && a.Country == ""
}
