package email

import (
	"fmt"

	"github.com/emersion/go-imap"
)

// MarkMessageFlag marks a message with a specific flag
func (c *Client) MarkMessageFlag(uid uint32, folder, flag string, value bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	// Select the folder
	_, err := c.imap.Select(folder, false)
	if err != nil {
		return fmt.Errorf("failed to select folder: %w", err)
	}

	// Create sequence set for the message
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// Determine if we're adding or removing the flag
	var operation imap.FlagsOp
	if value {
		operation = imap.AddFlags
	} else {
		operation = imap.RemoveFlags
	}

	// Perform the flag update
	item := imap.FormatFlagsOp(operation, true)
	flags := []interface{}{flag}

	err = c.imap.UidStore(seqSet, item, flags, nil)
	if err != nil {
		return fmt.Errorf("failed to update flags: %w", err)
	}

	return nil
}

// GetMessageFlags retrieves flags for a specific message
func (c *Client) GetMessageFlags(uid uint32, folder string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil, fmt.Errorf("not connected")
	}

	// Select the folder
	_, err := c.imap.Select(folder, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select folder: %w", err)
	}

	// Create sequence set for the message
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// Fetch flags
	items := []imap.FetchItem{imap.FetchFlags}
	messages := make(chan *imap.Message, 1)
	err = c.imap.UidFetch(seqSet, items, messages)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch flags: %w", err)
	}

	// Get the message
	msg := <-messages
	if msg == nil {
		return nil, fmt.Errorf("message not found")
	}

	// Simply return the flags as they're already strings
	return msg.Flags, nil
}
