// handlers/api/idle.go
//
// IMAP IDLE watcher.  Uses emersion/go-imap-idle to detect new messages in
// INBOX in real time (with a transparent NOOP-poll fallback for servers that
// do not support IDLE).
//
// Usage:
//
//	stop := make(chan struct{})
//	go client.WatchInbox(stop, func(email models.Email) {
//	    fmt.Printf("new mail from %s: %s\n", email.From, email.Subject)
//	})
//	// …later…
//	close(stop) // terminates the watcher
package api

import (
	"fmt"
	"log"
	"time"

	"github.com/emersion/go-imap"
	idle "github.com/emersion/go-imap-idle"
	imapClient "github.com/emersion/go-imap/client"
	"lilmail/models"
)

// WatchInbox selects INBOX on the receiver's underlying IMAP connection, then
// watches for new messages using IMAP IDLE (with a NOOP-poll fallback).
//
// When the message count in INBOX increases, WatchInbox fetches the envelope of
// the newest message and calls onNewMail.  The function blocks until stop is
// closed or the IMAP connection is terminated.
//
// The caller is responsible for closing the *Client when done — typically by
// deferring client.Close() in the goroutine that calls WatchInbox.
func (c *Client) WatchInbox(stop <-chan struct{}, onNewMail func(email models.Email)) error {
	raw := c.IMAPClient()

	// Select INBOX so that the server sends EXISTS updates.
	mbox, err := raw.Select("INBOX", false)
	if err != nil {
		return fmt.Errorf("idle: select INBOX: %w", err)
	}
	prevCount := mbox.Messages

	// Register a channel for unsolicited server updates.
	updates := make(chan imapClient.Update, 16)
	raw.Updates = updates

	idleClient := idle.NewClient(raw)

	for {
		// IDLE owns the connection until it is terminated, so a FETCH issued while
		// it runs just blocks (the watcher then never reported any new mail). Run
		// IDLE until the server announces more messages, end it, fetch, resume.
		idleStop := make(chan struct{})
		idleDone := make(chan error, 1)
		go func() { idleDone <- idleClient.IdleWithFallback(idleStop, time.Minute) }()

		var newCount uint32
	wait:
		for {
			select {
			case update, ok := <-updates:
				if !ok {
					close(idleStop)
					<-idleDone
					return nil
				}
				if mb, isMailbox := update.(*imapClient.MailboxUpdate); isMailbox {
					if n := mb.Mailbox.Messages; n > prevCount {
						newCount = n
						break wait
					} else {
						prevCount = n
					}
				}
			case err := <-idleDone:
				if err != nil {
					return fmt.Errorf("idle: %w", err)
				}
				return nil
			case <-stop:
				close(idleStop)
				<-idleDone
				return nil
			}
		}

		close(idleStop)
		if err := <-idleDone; err != nil {
			return fmt.Errorf("idle: %w", err)
		}
		emails, err := c.fetchEnvelopes(raw, prevCount+1, newCount)
		prevCount = newCount
		if err != nil {
			log.Printf("idle: fetchEnvelopes: %v", err)
			continue
		}
		for _, email := range emails {
			onNewMail(email)
		}
	}
}

// fetchEnvelopes fetches lightweight envelopes (From, Subject, Date) for
// messages fromSeq..toSeq in the currently selected mailbox.
func (c *Client) fetchEnvelopes(raw *imapClient.Client, fromSeq, toSeq uint32) ([]models.Email, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddRange(fromSeq, toSeq)

	items := []imap.FetchItem{imap.FetchEnvelope}
	ch := make(chan *imap.Message, int(toSeq-fromSeq+1))
	done := make(chan error, 1)
	go func() {
		done <- raw.Fetch(seqSet, items, ch)
	}()

	var emails []models.Email
	for msg := range ch {
		if msg.Envelope == nil {
			continue
		}
		env := msg.Envelope
		from := ""
		if len(env.From) > 0 {
			addr := env.From[0]
			if addr.PersonalName != "" {
				from = addr.PersonalName
			} else {
				from = addr.MailboxName + "@" + addr.HostName
			}
		}
		emails = append(emails, models.Email{
			From:    from,
			Subject: env.Subject,
			Date:    env.Date,
		})
	}
	if err := <-done; err != nil {
		return emails, err
	}
	return emails, nil
}
