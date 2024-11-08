// handlers/api/client.go
package api

import (
	"fmt"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// Client represents an IMAP client wrapper
type Client struct {
	client *client.Client
}

// NewClient creates a new IMAP client
func NewClient(server string, port int, email, password string) (*Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, port), nil)
	if err != nil {
		return nil, fmt.Errorf("connection error: %v", err)
	}

	err = c.Login(email, password)
	if err != nil {
		c.Logout()
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
