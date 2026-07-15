package models

import (
	"time"
)

type Email struct {
	ID             string       `json:"id"`
	From           string       `json:"from"`
	FromName       string       `json:"fromName,omitempty"`
	To             string       `json:"to"`
	ToNames        []string     `json:"toNames,omitempty"`
	Cc             string       `json:"cc,omitempty"`
	Subject        string       `json:"subject"`
	Preview        string       `json:"preview"`
	Body           string       `json:"body"`           // Plain text
	HTML           string       `json:"html,omitempty"` // Raw HTML body; auto-escaped by html/template
	Date           time.Time    `json:"date"`
	HasAttachments bool         `json:"hasAttachments"`
	Flags          []string     `json:"flags,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	// Threading headers (JWZ)
	MessageID  string   `json:"messageId,omitempty"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References []string `json:"references,omitempty"`
	// Multi-account: source account metadata (empty when single-account mode)
	AccountEmail string `json:"accountEmail,omitempty"`
	AccountLabel string `json:"accountLabel,omitempty"`
	AccountColor string `json:"accountColor,omitempty"`
	// Invite is populated when the message carries a text/calendar iMIP part
	// (iTIP). Nil for ordinary mail. Drives the RSVP card in the reading pane.
	Invite *CalendarInvite `json:"invite,omitempty"`
	// Auth carries the inbound sender-authentication results (SPF/DKIM/DMARC)
	// parsed from the message's Authentication-Results header. Nil when the header
	// is absent or unparseable. Read-only; drives the client's "verified sender"
	// / "why in spam" badge.
	Auth *AuthResults `json:"auth,omitempty"`
	// Unsubscribe carries the parsed List-Unsubscribe (RFC 2369) targets plus the
	// RFC 8058 one-click flag, when the message advertises them. Nil for ordinary
	// mail. Read-only; drives the reading pane's one-click Unsubscribe button. The
	// client validates the scheme and only POSTs to the one-click target — it
	// never auto-follows a hostile URL.
	Unsubscribe *Unsubscribe `json:"unsubscribe,omitempty"`
	// Brand carries a VERIFIED sender brand logo (BIMI) — populated ONLY when the
	// message passed DMARC (alignment implied) AND the From domain publishes a BIMI
	// record whose logo we fetched, screened, and sanitized. Nil otherwise. It is a
	// privacy-safe sender image (keyed on the SENDER's own DNS + authentication,
	// leaking nothing about the recipient) and doubles as an anti-phishing signal:
	// a logo appears ONLY for a genuinely authenticated sender (fail-closed). The
	// logo is a sanitized SVG data URI safe to render in an <img> (image context —
	// no script execution). Never trusted from a client; set server-side only.
	Brand *BrandIndicator `json:"brand,omitempty"`
}

// BrandIndicator is a verified BIMI brand logo for a sender domain. Present only
// when DMARC passed and the domain published a BIMI record with a usable logo.
type BrandIndicator struct {
	// Domain is the DMARC-aligned From domain the logo was verified for.
	Domain string `json:"domain"`
	// Logo is a sanitized SVG rendered as a `data:image/svg+xml;base64,…` URI. It
	// is safe to place in an <img src> — an SVG in image context cannot execute
	// script or load external resources — and it additionally passed a strict
	// server-side sanitizer (no <script>/<foreignObject>/event handlers/external
	// references), so a non-compliant logo yields NO indicator (fail-closed).
	Logo string `json:"logo"`
	// VMC reports whether the BIMI record also referenced a Verified Mark
	// Certificate (the `a=` tag). Informational: a VMC means a CA attested the
	// mark, a stronger trust tier than a self-asserted logo. We do not yet validate
	// the certificate chain, so this only reflects the record, not a verification.
	VMC bool `json:"vmc,omitempty"`
}

// Unsubscribe is the distilled List-Unsubscribe / List-Unsubscribe-Post value
// pair (RFC 2369 + RFC 8058) advertised by bulk senders. lilmail parses it
// read-only from the delivered message headers; it never itself unsubscribes.
//
// A List-Unsubscribe header holds one or more angle-bracketed URIs, e.g.
//
//	List-Unsubscribe: <https://example.com/u/abc>, <mailto:unsub@example.com?subject=unsub>
//
// The client prefers the https one-click POST target (present only when the
// sender also sent `List-Unsubscribe-Post: List-Unsubscribe=One-Click`, per RFC
// 8058); otherwise it opens the http(s) landing page or the mailto: in a new
// window. Only http/https/mailto schemes are surfaced — anything else is dropped
// so a hostile scheme can never ride through to the client.
type Unsubscribe struct {
	// HTTPURL is the first http/https URI from List-Unsubscribe, or "" if none.
	HTTPURL string `json:"httpUrl,omitempty"`
	// MailtoURL is the first mailto: URI from List-Unsubscribe, or "" if none.
	MailtoURL string `json:"mailtoUrl,omitempty"`
	// OneClick is true when the sender advertised RFC 8058 one-click unsubscribe
	// (`List-Unsubscribe-Post: List-Unsubscribe=One-Click`) AND an HTTPURL exists,
	// meaning the client may POST `List-Unsubscribe=One-Click` to HTTPURL directly
	// without the user leaving the mail app.
	OneClick bool `json:"oneClick,omitempty"`
}

// AuthResults is the distilled SPF/DKIM/DMARC verdict from a message's
// Authentication-Results header (RFC 8601), as stamped by the RECEIVING mail
// server (the boundary MTA that authenticated the message on delivery). It is
// read-only metadata for a "verified sender" / "why in spam" badge — lilmail does
// not itself perform the checks; it surfaces the trusted receiver's verdict.
//
// Each verdict is the raw result token, lower-cased: "pass", "fail", "softfail",
// "neutral", "none", "temperror", "permerror", "" (absent). DKIMDomain is the
// header.d= of the (first) DKIM signature when present.
type AuthResults struct {
	SPF        string `json:"spf,omitempty"`
	DKIM       string `json:"dkim,omitempty"`
	DMARC      string `json:"dmarc,omitempty"`
	DKIMDomain string `json:"dkimDomain,omitempty"`
	// Raw is the verbatim Authentication-Results header value, for a client that
	// wants to show the full detail on demand.
	Raw string `json:"raw,omitempty"`
}

// Attachment is one downloadable MIME part of a message. In a message listing
// only the metadata is populated (Content is fetched on demand by the download
// route), so a client can render an attachment list + download links without
// pulling any bytes.
//
// PartID is the raw IMAP MIME part path (e.g. "2.1"); it is what the JSON API
// download route consumes:  GET /v1/messages/:uid/attachments/:partId?folder=.
// ID is the opaque encoded token (base64 of folder\0uid\0part) used by the
// HTMX web download route instead. Content is never serialized to JSON — it is
// an in-process carrier for the on-demand download path only.
type Attachment struct {
	ID          string `json:"id"`
	PartID      string `json:"partId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"isInline"`
	Content     []byte `json:"-"`
}

// Thread represents a JWZ conversation group. Root is the earliest (or
// synthetic root) message; Messages is all messages in the thread sorted by
// date ascending; Count is len(Messages).
type Thread struct {
	Root     Email
	Messages []Email
	Count    int
	// Latest caches the most-recent message for sorting / display.
	Latest Email
}
