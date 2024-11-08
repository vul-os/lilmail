package handlers

import (
	"fmt"
	"io"
	"lilmail/models"
	"lilmail/utils"
	"path/filepath"
	"strings"

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

// IMAPLogin creates a new IMAP client connection
func IMAPLogin(server, username, password, cacheFolder string) (*client.Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, 993), nil)
	if err != nil {
		return nil, fmt.Errorf("connection error: %v", err)
	}

	err = c.Login(username, password)
	if err != nil {
		c.Logout()
		return nil, fmt.Errorf("login error: %v", err)
	}

	return c, nil
}

// FetchFolders retrieves all mailbox folders
func (c *Client) FetchFolders() ([]*imap.MailboxInfo, error) {
	mailboxChan := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.client.List("", "*", mailboxChan)
	}()

	var mailboxes []*imap.MailboxInfo
	for mb := range mailboxChan {
		mailboxes = append(mailboxes, mb)
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("error fetching folders: %v", err)
	}

	return mailboxes, nil
}

// FetchAndCacheData fetches folders and emails, storing them in cache
func FetchAndCacheData(c *client.Client, cacheFolder string) error {
	// Create a channel to receive mailbox info
	ch := make(chan *imap.MailboxInfo, 10)

	// Fetch folder structure (mailboxes)
	err := c.List("", "*", ch)
	if err != nil {
		return fmt.Errorf("error listing folders: %v", err)
	}

	var mailboxesList []*imap.MailboxInfo
	for mailbox := range ch {
		mailboxesList = append(mailboxesList, mailbox)
	}

	// Cache folder structure
	err = utils.SaveCache(filepath.Join(cacheFolder, "folders.json"), mailboxesList)
	if err != nil {
		return fmt.Errorf("error caching folders: %v", err)
	}

	// Select INBOX
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return fmt.Errorf("error selecting inbox: %v", err)
	}

	// Fetch emails (example: first 10 emails from "INBOX")
	from := uint32(1)
	to := uint32(10)
	if mbox.Messages < 10 {
		to = mbox.Messages
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	messages := make(chan *imap.Message, 10)
	section := &imap.BodySectionName{}

	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqSet, []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchFlags,
			imap.FetchBodyStructure,
			section.FetchItem(),
		}, messages)
	}()

	var emails []models.Email
	for msg := range messages {
		email, err := processMessage(msg)
		if err != nil {
			fmt.Printf("Error processing message: %v\n", err)
			continue
		}
		emails = append(emails, email)
	}

	// Wait for the fetch to complete
	if err := <-done; err != nil {
		return fmt.Errorf("fetch error: %v", err)
	}

	// Cache emails
	err = utils.SaveCache(filepath.Join(cacheFolder, "emails.json"), emails)
	if err != nil {
		return fmt.Errorf("error caching emails: %v", err)
	}

	return nil
}

// processMessage converts an IMAP message to our Email model
func processMessage(msg *imap.Message) (models.Email, error) {
	var email models.Email

	// Process From address
	if len(msg.Envelope.From) > 0 {
		email.From = msg.Envelope.From[0].Address()
		email.FromName = msg.Envelope.From[0].PersonalName
	}

	// Process To addresses
	if len(msg.Envelope.To) > 0 {
		var toAddresses []string
		for _, addr := range msg.Envelope.To {
			toAddresses = append(toAddresses, addr.Address())
		}
		email.To = strings.Join(toAddresses, ", ") // Fix: Join the slice into a single string
	}

	// Set other fields
	email.Subject = msg.Envelope.Subject
	email.Date = msg.Envelope.Date

	// Process body
	for _, literal := range msg.Body {
		if literal != nil {
			body, err := io.ReadAll(literal)
			if err != nil {
				return email, fmt.Errorf("error reading message body: %v", err)
			}
			email.Body = string(body)
			break // We'll just use the first body part for this example
		}
	}

	// Set message ID
	email.ID = fmt.Sprintf("%d", msg.Uid)
	email.Flags = msg.Flags

	return email, nil
}
