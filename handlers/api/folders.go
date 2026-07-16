// handlers/api/folders.go — mailbox (folder) create/delete + special-use
// discovery for the Snoozed and Junk/Spam folders.
//
// These sit alongside the existing Drafts/Trash discovery in draft.go and reuse
// the same IMAP client. Folder create/delete backs the /v1 label CRUD surface;
// the Snoozed/Junk discovery backs the snooze move and the report-spam action.
package api

import (
	"fmt"
	"strings"

	imap "github.com/emersion/go-imap"
)

// CreateMailbox creates an IMAP mailbox (folder) with the given name. It is
// idempotent-ish: a server that reports the mailbox already exists is treated as
// success, so a client creating a label that maps to an existing folder does not
// error.
func (c *Client) CreateMailbox(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("mailbox name is required")
	}
	if err := c.client.Create(name); err != nil {
		// Most servers return "already exists" (or [ALREADYEXISTS]); treat as OK.
		if strings.Contains(strings.ToLower(err.Error()), "exist") {
			return nil
		}
		return fmt.Errorf("create mailbox %q: %w", name, err)
	}
	// Subscribe so the folder shows up in LSUB-based clients too. Best-effort.
	_ = c.client.Subscribe(name)
	return nil
}

// DeleteMailbox deletes an IMAP mailbox (folder) by name. Deleting a mailbox
// discards the messages it holds — the /v1 handler guards against deleting the
// system folders (INBOX/Sent/Spam/Trash/…) before calling this.
func (c *Client) DeleteMailbox(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("mailbox name is required")
	}
	_ = c.client.Unsubscribe(name) // best-effort; ignore if not subscribed
	if err := c.client.Delete(name); err != nil {
		return fmt.Errorf("delete mailbox %q: %w", name, err)
	}
	return nil
}

// discoverSpecialFolder finds a mailbox by a set of IMAP special-use attributes
// (e.g. \Junk) with a fallback to common name guesses. When createName is
// non-empty and nothing is found, it creates that mailbox and returns it, so a
// feature that needs the folder (snooze) always has a destination.
func (c *Client) discoverSpecialFolder(specialUse []string, nameGuesses []string, createName string) (string, error) {
	mailboxChan := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- c.client.List("", "*", mailboxChan)
	}()

	var bySpecialUse string
	var candidates []string
	for mb := range mailboxChan {
		for _, attr := range mb.Attributes {
			for _, su := range specialUse {
				if strings.EqualFold(attr, su) && bySpecialUse == "" {
					bySpecialUse = mb.Name
				}
			}
		}
		lc := strings.ToLower(mb.Name)
		for _, g := range nameGuesses {
			if lc == g || strings.HasSuffix(lc, "/"+g) {
				candidates = append(candidates, mb.Name)
			}
		}
	}
	if err := <-done; err != nil {
		return "", fmt.Errorf("LIST error: %w", err)
	}

	if bySpecialUse != "" {
		return bySpecialUse, nil
	}
	if len(candidates) > 0 {
		return candidates[0], nil
	}
	if createName != "" {
		if err := c.CreateMailbox(createName); err != nil {
			return "", err
		}
		return createName, nil
	}
	return "", fmt.Errorf("could not locate folder")
}

// DiscoverSnoozedFolder locates (or creates) the Snoozed folder. lilmail maps
// the "snoozed" label to a "Snoozed" IMAP folder; snoozing moves the message
// there. If no such folder exists on the account, it is created so snooze always
// has a destination.
func (c *Client) DiscoverSnoozedFolder() (string, error) {
	return c.discoverSpecialFolder(nil, []string{"snoozed"}, "Snoozed")
}

// DiscoverJunkFolder locates the Junk/Spam folder by the \Junk special-use, then
// common name guesses. Unlike Snoozed it is not auto-created — a mailbox without
// a Spam folder is surfaced as an error so the report-spam action degrades
// clearly rather than silently creating a folder the server does not expect.
func (c *Client) DiscoverJunkFolder() (string, error) {
	return c.discoverSpecialFolder([]string{`\Junk`}, []string{"junk", "spam", "junk email", "bulk mail"}, "")
}
