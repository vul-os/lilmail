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
	// ThreadID is the CANONICAL server-side conversation id computed by vulos-mail
	// at ingest and surfaced (brokered) via /v1/threads and ?threaded=1 on the
	// message list. Empty for non-vulos-mail-hosted accounts (standalone/session
	// lilmail, plain Gmail/IMAP) — those clients fall back to their own JWZ
	// union-find over MessageID/InReplyTo/References. Never trusted from a client.
	ThreadID string `json:"threadId,omitempty"`
	// Category is the Gmail-style inbox tab (primary|social|promotions|updates|
	// forums) this message was classified into by vulos-mail at ingest, surfaced
	// (brokered) via ?category filtering on the message list and re-categorization.
	// Empty for non-vulos-mail-hosted accounts (standalone/session lilmail, plain
	// Gmail/IMAP) — those clients show a single Primary tab. Never trusted from a
	// client; it is stamped server-side only.
	Category string `json:"category,omitempty"`
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
