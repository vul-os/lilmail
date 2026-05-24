package models

import (
	"html/template"
	"time"
)

type Email struct {
	ID             string        `json:"id"`
	From           string        `json:"from"`
	FromName       string        `json:"fromName,omitempty"`
	To             string        `json:"to"`
	ToNames        []string      `json:"toNames,omitempty"`
	Cc             string        `json:"cc,omitempty"`
	Subject        string        `json:"subject"`
	Preview        string        `json:"preview"`
	Body           string        `json:"body"` // Plain text
	HTML           template.HTML // Not string
	Date           time.Time     `json:"date"`
	HasAttachments bool          `json:"hasAttachments"`
	Flags          []string      `json:"flags,omitempty"`
	Attachments    []Attachment  `json:"attachments,omitempty"`
	// Threading headers (JWZ)
	MessageID  string   `json:"messageId,omitempty"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References []string `json:"references,omitempty"`
}

// type Attachment struct {
// 	Filename    string
// 	ContentType string
// 	Size        int
// 	Content     []byte
// }

type Attachment struct {
	ID          string
	Filename    string
	ContentType string
	Content     []byte

	Size int
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
