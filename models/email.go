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
