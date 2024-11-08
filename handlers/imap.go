package handlers

import (
	"fmt"
	"io"
	"lilmail/models"
	"lilmail/utils"
	"path/filepath"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// Login to IMAP server
func IMAPLogin(server, username, password, cacheFolder string) (*client.Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, 993), nil)
	if err != nil {
		return nil, err
	}

	err = c.Login(username, password)
	if err != nil {
		c.Logout()
		return nil, err
	}

	return c, nil
}

// Fetch folders and emails, store them in cache
func FetchAndCacheData(c *client.Client, cacheFolder string) error {
	// Create a channel to receive mailbox info
	ch := make(chan *imap.MailboxInfo, 10)

	// Fetch folder structure (mailboxes)
	err := c.List("", "*", ch)
	if err != nil {
		return err
	}

	var mailboxesList []*imap.MailboxInfo
	for mailbox := range ch {
		mailboxesList = append(mailboxesList, mailbox)
	}

	// Cache folder structure
	err = utils.SaveCache(filepath.Join(cacheFolder, "folders.json"), mailboxesList)
	if err != nil {
		return err
	}

	// Select INBOX
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return err
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
		done <- c.Fetch(seqSet, []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}, messages)
	}()

	var emails []models.Email
	for msg := range messages {
		var emailBody string

		// Properly handle the literal body
		for _, literal := range msg.Body {
			if literal != nil {
				body, err := io.ReadAll(literal)
				if err != nil {
					fmt.Printf("Error reading message body: %v\n", err)
					continue
				}
				emailBody = string(body)
				break // We'll just use the first body part for this example
			}
		}

		// Safely handle the From address
		var fromAddress string
		if len(msg.Envelope.From) > 0 {
			fromAddress = msg.Envelope.From[0].Address()
		}

		emails = append(emails, models.Email{
			From:    fromAddress,
			Subject: msg.Envelope.Subject,
			Body:    emailBody,
		})
	}

	// Wait for the fetch to complete
	if err := <-done; err != nil {
		return fmt.Errorf("fetch error: %v", err)
	}

	// Cache emails
	err = utils.SaveCache(filepath.Join(cacheFolder, "emails.json"), emails)
	if err != nil {
		return err
	}

	return nil
}

type Client struct {
	client *client.Client
}

func NewClient(server string, port int, email, password string) (*Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, port), nil)
	if err != nil {
		return nil, err
	}

	err = c.Login(email, password)
	if err != nil {
		c.Logout()
		return nil, err
	}

	return &Client{client: c}, nil
}

func (c *Client) Close() error {
	return c.client.Logout()
}

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

	return mailboxes, <-done
}
