package handlers

import (
	"fmt"

	"github.com/emersion/go-imap"
)

// DeleteMessage deletes a specific message by its UID from a specific folder
func (c *Client) DeleteMessage(folderName, uid string) error {
	// Convert string UID to uint32
	uidNum, err := parseUID(uid)
	if err != nil {
		return fmt.Errorf("invalid UID: %v", err)
	}

	// Select the specific folder
	_, err = c.client.Select(folderName, false) // false for write mode
	if err != nil {
		return fmt.Errorf("error selecting folder %s: %v", folderName, err)
	}

	// Create sequence set for the message
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uidNum)

	// Add the \Deleted flag
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}

	err = c.client.UidStore(seqSet, item, flags, nil)
	if err != nil {
		return fmt.Errorf("error marking message as deleted: %v", err)
	}

	// Expunge to permanently remove the message
	err = c.client.Expunge(nil)
	if err != nil {
		return fmt.Errorf("error expunging mailbox: %v", err)
	}

	return nil
}
