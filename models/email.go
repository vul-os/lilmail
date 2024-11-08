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
	Body           string       `json:"body"` // Plain text
	HTML           string       `json:"html"` // HTML version
	Date           time.Time    `json:"date"`
	HasAttachments bool         `json:"hasAttachments"`
	IsHTML         bool         `json:"isHTML"`
	Flags          []string     `json:"flags,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
}

type Attachment struct {
	Filename    string
	ContentType string
	Size        int
	Content     []byte
}
