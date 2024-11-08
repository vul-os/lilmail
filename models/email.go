package models

import (
	"time"
)

// models/email.go
type Email struct {
	ID          string
	Subject     string
	From        string
	FromName    string
	To          string
	ToNames     []string
	Cc          string
	Date        time.Time
	Body        string
	HTML        string
	Flags       []string
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	Size        int
	Content     []byte
}
