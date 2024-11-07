package models

import (
	"time"
)

// Address represents an email address with optional name
type Address struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Message represents an email message
type Message struct {
	UID       uint32    `json:"uid"`
	Subject   string    `json:"subject"`
	From      Address   `json:"from"`
	To        []Address `json:"to"`
	Cc        []Address `json:"cc,omitempty"`
	Bcc       []Address `json:"bcc,omitempty"`
	Date      time.Time `json:"date"`
	Body      string    `json:"body,omitempty"`
	HTMLBody  string    `json:"html_body,omitempty"`
	Seen      bool      `json:"seen"`
	Flagged   bool      `json:"flagged"`
	Answered  bool      `json:"answered"`
	Draft     bool      `json:"draft"`
	Deleted   bool      `json:"deleted"`
	Size      uint32    `json:"size"`
	Snippet   string    `json:"snippet,omitempty"`
	MessageID string    `json:"message_id"`

	// Attachments if any
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment represents an email attachment
type Attachment struct {
	ID          string `json:"id"`           // Unique identifier for the attachment
	Filename    string `json:"filename"`     // Original filename
	ContentType string `json:"content_type"` // MIME type
	Size        int64  `json:"size"`         // Size in bytes
	Inline      bool   `json:"inline"`       // Whether this is an inline attachment
	ContentID   string `json:"content_id"`   // Content-ID for inline images
}

// Folder represents an email folder/mailbox
type Folder struct {
	Name   string
	Unread int
}

// FetchOptions represents options for fetching messages
type FetchOptions struct {
	Folder    string    // Folder to fetch from
	Start     uint32    // Start sequence number
	Count     uint32    // Number of messages to fetch
	FetchBody bool      // Whether to fetch message bodies
	UseCache  bool      // Whether to use cached messages
	Since     time.Time // Only fetch messages since this time (optional)
}

// SearchOptions represents options for searching messages
type SearchOptions struct {
	Folder  string    // Folder to search in
	Query   string    // Search query
	From    string    // From address
	To      string    // To address
	Subject string    // Subject contains
	Before  time.Time // Before date
	After   time.Time // After date
	Seen    *bool     // Seen flag (nil for any)
	Flagged *bool     // Flagged/starred (nil for any)
}

// FolderStatus contains folder metadata
type FolderStatus struct {
	Messages    uint32 // Total number of messages
	Recent      uint32 // Number of recent messages
	Unseen      uint32 // Number of unseen messages
	UIDNext     uint32 // Next predicted UID
	UIDValidity uint32 // UID validity value
}
