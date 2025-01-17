// handlers/api/client.go
package api

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// Client represents an IMAP client wrapper
type Client struct {
	client   *client.Client
	username string // Add username field
}

// NewClient creates a new IMAP client
func NewClient(server string, port int, email, password string) (*Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, port), nil)
	if err != nil {
		log.Printf("DialTLS %s:%d connection err: %v", server, port, err)
		return nil, fmt.Errorf("connection error: %v", err)
	}

	err = c.Login(email, password)
	if err != nil {
		c.Logout()
		log.Printf("IMAP Login %s/xxx login err: %v", email, err)
		return nil, fmt.Errorf("login error: %v", err)
	}

	return &Client{client: c}, nil
}

// Close closes the IMAP connection
func (c *Client) Close() error {
	return c.client.Logout()
}

// FetchFolders retrieves all mailbox folders
func (c *Client) FetchFolders() ([]*MailboxInfo, error) {
	mailboxChan := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.client.List("", "*", mailboxChan)
	}()

	var mailboxes []*MailboxInfo
	for mb := range mailboxChan {
		mailboxes = append(mailboxes, &MailboxInfo{
			Name:       mb.Name,
			Delimiter:  mb.Delimiter,
			Attributes: mb.Attributes,
		})
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("error fetching folders: %v", err)
	}

	return mailboxes, nil
}

// SelectFolder selects a mailbox/folder
func (c *Client) SelectFolder(folderName string, readOnly bool) (*imap.MailboxStatus, error) {
	return c.client.Select(folderName, readOnly)
}

type MailboxInfo struct {
	Attributes  []string `json:"attributes"`
	Delimiter   string   `json:"delimiter"`
	Name        string   `json:"name"`
	UnreadCount int      `json:"unreadCount,omitempty"`
}

// parseUID converts a string UID to uint32
func parseUID(uid string) (uint32, error) {
	var uidNum uint32
	_, err := fmt.Sscanf(uid, "%d", &uidNum)
	if err != nil {
		return 0, err
	}
	return uidNum, nil
}

// Add this method to your existing Client struct
func (c *Client) SaveToSent(to, subject, body string) error {
	// Try different common names for Sent folder
	sentFolders := []string{"Sent", "Sent Items", "Sent Mail"}

	var selectedFolder string
	for _, folder := range sentFolders {
		if _, err := c.client.Select(folder, false); err == nil {
			selectedFolder = folder
			break
		}
	}

	if selectedFolder == "" {
		return fmt.Errorf("could not find Sent folder")
	}

	// Format the message
	message := fmt.Sprintf("From: %s\r\n"+
		"To: %s\r\n"+
		"Subject: %s\r\n"+
		"Date: %s\r\n"+
		"Content-Type: text/plain; charset=UTF-8\r\n"+
		"\r\n"+
		"%s", c.username, to, subject,
		time.Now().Format(time.RFC1123Z), body)

	// Append the message to the Sent folder
	return c.client.Append(selectedFolder, nil, time.Now(), strings.NewReader(message))
}
