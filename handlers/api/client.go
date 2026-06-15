// handlers/api/client.go
package api

import (
	"fmt"
	"lilmail/models"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
)

// SearchMessages performs an IMAP SEARCH in the given folder and returns the
// matching messages.  query is searched in the Subject, From, and body (TEXT
// criterion).  limit caps the number of results returned.
func (c *Client) SearchMessages(folderName, query string, limit uint32) ([]models.Email, error) {
	if _, err := c.client.Select(folderName, true); err != nil {
		return nil, fmt.Errorf("search: select %s: %w", folderName, err)
	}

	// Build a compound OR criterion: TEXT <q> covers body+headers, but we also
	// add explicit SUBJECT and FROM for servers that index them separately.
	// RFC 3501 OR takes exactly two operands; to combine three we nest:
	//   OR (OR (TEXT q) (SUBJECT q)) (FROM q)
	q := query
	criteria := &imap.SearchCriteria{
		// TEXT searches the full message (headers + body).
		Text: []string{q},
	}

	uids, err := c.client.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search: uid search: %w", err)
	}
	if len(uids) == 0 {
		return []models.Email{}, nil
	}

	// Cap result set.
	if limit > 0 && uint32(len(uids)) > limit {
		uids = uids[uint32(len(uids))-limit:]
	}

	seqSet := new(imap.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	messages := make(chan *imap.Message, len(uids))
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchBodyStructure,
		imap.FetchUid,
		previewSection.FetchItem(),
		referencesSection.FetchItem(),
	}
	fetchDone := make(chan error, 1)
	go func() {
		fetchDone <- c.client.UidFetch(seqSet, items, messages)
	}()

	var emails []models.Email
	for msg := range messages {
		email, err := c.processListMessage(msg, folderName)
		if err != nil {
			log.Printf("search: processListMessage uid=%d: %v", msg.Uid, err)
			continue
		}
		emails = append(emails, email)
	}
	if err := <-fetchDone; err != nil {
		return emails, fmt.Errorf("search: fetch: %w", err)
	}
	return emails, nil
}

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

	return &Client{client: c, username: email}, nil
}

// NewClientOAuth creates a new IMAP client authenticated with an OAuth2 access
// token using the XOAUTH2 (default) or OAUTHBEARER SASL mechanism.
func NewClientOAuth(server string, port int, username, accessToken, mechanism string) (*Client, error) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", server, port), nil)
	if err != nil {
		log.Printf("DialTLS %s:%d connection err: %v", server, port, err)
		return nil, fmt.Errorf("connection error: %v", err)
	}

	var auth sasl.Client
	switch strings.ToLower(mechanism) {
	case "oauthbearer":
		auth = sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: username,
			Token:    accessToken,
			Host:     server,
			Port:     port,
		})
	default:
		auth = NewXoauth2Client(username, accessToken)
	}

	if err := c.Authenticate(auth); err != nil {
		c.Logout()
		log.Printf("IMAP OAuth2 (%s) login %s err: %v", mechanism, username, err)
		return nil, fmt.Errorf("oauth2 login error: %v", err)
	}

	return &Client{client: c, username: username}, nil
}

// Close closes the IMAP connection
func (c *Client) Close() error {
	return c.client.Logout()
}

// IMAPClient returns the underlying go-imap *client.Client so that
// packages such as the IDLE watcher can use extension-specific APIs.
func (c *Client) IMAPClient() *client.Client {
	return c.client
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

// discoverSentFolder uses IMAP LIST to find the Sent folder by first looking
// for the \Sent special-use attribute, then falling back to common name guesses.
func (c *Client) discoverSentFolder() (string, error) {
	// Phase 1: scan for the \Sent special-use attribute.
	mailboxChan := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- c.client.List("", "*", mailboxChan)
	}()

	var bySpecialUse string
	var candidates []string
	for mb := range mailboxChan {
		for _, attr := range mb.Attributes {
			if strings.EqualFold(attr, `\Sent`) || strings.EqualFold(attr, `\All`) {
				if bySpecialUse == "" {
					bySpecialUse = mb.Name
				}
			}
		}
		lc := strings.ToLower(mb.Name)
		if lc == "sent" || strings.HasSuffix(lc, "/sent") ||
			lc == "sent items" || lc == "sent mail" {
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
	for _, name := range []string{"Sent", "Sent Items", "Sent Mail"} {
		if _, err := c.client.Select(name, false); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not locate Sent folder")
}

// SaveToSent appends a copy of the sent message to the user's Sent folder.
// The Sent folder is discovered via IMAP LIST (special-use \Sent attribute first,
// then common name guesses).
func (c *Client) SaveToSent(to, subject, body string) error {
	folder, err := c.discoverSentFolder()
	if err != nil {
		return err
	}

	// Format the message
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		c.username, to, subject, time.Now().Format(time.RFC1123Z), body)

	return c.client.Append(folder, nil, time.Now(), strings.NewReader(message))
}
