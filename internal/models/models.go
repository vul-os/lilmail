// internal/models/email.go
package models

import (
	"encoding/json"
	"time"
)

// Email represents a single email message with all its metadata
type Email struct {
	UID       uint32    `json:"uid"`
	MessageID string    `json:"message_id"`
	Folder    string    `json:"folder"`
	From      Address   `json:"from"`
	To        []Address `json:"to"`
	Cc        []Address `json:"cc"`
	Bcc       []Address `json:"bcc"`
	Subject   string    `json:"subject"`
	Body      Body      `json:"body"`
	Date      time.Time `json:"date"`
	Size      int64     `json:"size"`
	Flags     []string  `json:"flags"`
	HasAttach bool      `json:"has_attachments"`
	CacheKey  string    `json:"cache_key"`
	Encrypted bool      `json:"encrypted"`
}

// // Address represents an email address with optional name
// type Address struct {
// 	Name    string `json:"name"`
// 	Address string `json:"address"`
// }

// Body contains both HTML and plain text versions of the email
type Body struct {
	HTML     string           `json:"html"`
	Text     string           `json:"text"`
	Attached []AttachmentMeta `json:"attachments"`
}

// AttachmentMeta holds metadata about an email attachment
type AttachmentMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	CacheKey    string `json:"cache_key"`
}

// EmailMetadata is a lightweight version of Email for listings
type EmailMetadata struct {
	UID       uint32    `json:"uid"`
	MessageID string    `json:"message_id"`
	From      Address   `json:"from"`
	Subject   string    `json:"subject"`
	Date      time.Time `json:"date"`
	Flags     []string  `json:"flags"`
	HasAttach bool      `json:"has_attachments"`
	Size      int64     `json:"size"`
}

// internal/models/user.go

// User represents an authenticated email user
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"display_name"`
	LastLogin    time.Time `json:"last_login"`
	LastSync     time.Time `json:"last_sync"`
	ServerConfig `json:"server_config"`
}

// ServerConfig holds the email server configuration
type ServerConfig struct {
	IMAPServer     string `json:"imap_server"`
	IMAPPort       int    `json:"imap_port"`
	Username       string `json:"username"`
	EncryptedPass  string `json:"encrypted_pass"`
	UseSSL         bool   `json:"use_ssl"`
	AutoDiscovered bool   `json:"auto_discovered"`
}

// Session represents a user's active session
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// internal/models/cache.go

// CacheEntry represents a cached item
type CacheEntry struct {
	Key         string    `json:"key"`
	Data        []byte    `json:"data"`
	Encrypted   bool      `json:"encrypted"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// FolderMetadata stores information about an email folder
type FolderMetadata struct {
	Name        string    `json:"name"`
	UIDValidity uint32    `json:"uid_validity"`
	LastUID     uint32    `json:"last_uid"`
	Count       int       `json:"count"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Serialization helpers
func (e *Email) MarshalBinary() ([]byte, error) {
	return json.Marshal(e)
}

func (e *Email) UnmarshalBinary(data []byte) error {
	return json.Unmarshal(data, e)
}

// Methods for EmailMetadata
func (e *Email) ToMetadata() EmailMetadata {
	return EmailMetadata{
		UID:       e.UID,
		MessageID: e.MessageID,
		From:      e.From,
		Subject:   e.Subject,
		Date:      e.Date,
		Flags:     e.Flags,
		HasAttach: e.HasAttach,
		Size:      e.Size,
	}
}

// Encryption info for cache entries
type EncryptionInfo struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Nonce     []byte `json:"nonce"`
}

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Server   string `json:"server,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// Add new type for credentials with all possible fields
type LoginCredentials struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	Server        string `json:"server,omitempty"`
	Port          int    `json:"port,omitempty"`
	UseSSL        bool   `json:"use_ssl,omitempty"`
	AllowInsecure bool   `json:"allow_insecure,omitempty"`
}
