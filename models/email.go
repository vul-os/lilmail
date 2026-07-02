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
