// handlers/api/draft.go — Drafts folder support: discover, save, list, restore.
package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

// discoverDraftsFolder uses IMAP LIST to find the Drafts folder by the \Drafts
// special-use attribute, then falls back to common name guesses, then SELECT probing.
func (c *Client) discoverDraftsFolder() (string, error) {
	mailboxChan := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- c.client.List("", "*", mailboxChan)
	}()

	var bySpecialUse string
	var candidates []string
	for mb := range mailboxChan {
		for _, attr := range mb.Attributes {
			if strings.EqualFold(attr, `\Drafts`) {
				if bySpecialUse == "" {
					bySpecialUse = mb.Name
				}
			}
		}
		lc := strings.ToLower(mb.Name)
		if lc == "drafts" || lc == "draft" || strings.HasSuffix(lc, "/drafts") {
			candidates = append(candidates, mb.Name)
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

	// Phase 2: try selecting common names in order.
	for _, name := range []string{"Drafts", "Draft", "DRAFTS"} {
		if _, err := c.client.Select(name, false); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not locate Drafts folder")
}

// SaveDraft appends a draft message to the user's Drafts folder with the \Draft
// and \Seen flags set. The raw RFC 2822 message bytes are provided by the caller
// so that the same MIME message can be built for both Send and SaveDraft.
func (c *Client) SaveDraft(rawMessage []byte) error {
	folder, err := c.discoverDraftsFolder()
	if err != nil {
		return err
	}

	flags := []string{imap.DraftFlag, imap.SeenFlag}
	return c.client.Append(folder, flags, time.Now(), strings.NewReader(string(rawMessage)))
}

// DiscoverDraftsFolder is the public wrapper around discoverDraftsFolder, so
// web handlers can find the Drafts folder name without accessing private methods.
func (c *Client) DiscoverDraftsFolder() (string, error) {
	return c.discoverDraftsFolder()
}

// DeleteDraft permanently removes a draft message from the Drafts folder by UID.
func (c *Client) DeleteDraft(uid string) error {
	folder, err := c.discoverDraftsFolder()
	if err != nil {
		return err
	}
	return c.DeleteMessageFromFolder(folder, uid)
}

// DeleteMessageFromFolder deletes a message by UID from an explicit folder.
func (c *Client) DeleteMessageFromFolder(folder, uid string) error {
	uidNum, err := parseUID(uid)
	if err != nil {
		return fmt.Errorf("invalid UID: %v", err)
	}

	if _, err := c.client.Select(folder, false); err != nil {
		return fmt.Errorf("error selecting folder %s: %v", folder, err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}
	if err := c.client.UidStore(seqSet, item, flags, nil); err != nil {
		return fmt.Errorf("error marking draft as deleted: %v", err)
	}
	if err := c.client.Expunge(nil); err != nil {
		return fmt.Errorf("error expunging drafts folder: %v", err)
	}
	return nil
}
