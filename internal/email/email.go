package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"

	"lilmail/internal/cache"
	"lilmail/internal/crypto"
	"lilmail/internal/models"

	"lilmail/pkg/concurrent"
)

var (
	ErrNotConnected = errors.New("not connected to mail server")
	ErrInvalidLogin = errors.New("invalid login credentials")
	ErrFetchFailed  = errors.New("failed to fetch messages")
)

type Client struct {
	imap   *client.Client
	cache  *cache.FileCache
	crypto *crypto.Manager
	pool   *concurrent.BatchProcessor

	config    *models.ServerConfig
	connected bool
	mu        sync.RWMutex
}

func NewClient(config *models.ServerConfig, cache *cache.FileCache, crypto *crypto.Manager) *Client {
	return &Client{
		config:    config,
		cache:     cache,
		crypto:    crypto,
		connected: false,
	}
}

func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	// Get decrypted password
	password, err := c.getDecryptedPassword()
	if err != nil {
		return fmt.Errorf("failed to decrypt password: %w", err)
	}

	serverAddr := fmt.Sprintf("%s:%d", c.config.IMAPServer, c.config.IMAPPort)
	fmt.Printf("Attempting to connect to %s (SSL: %v)\n", serverAddr, c.config.UseSSL)

	// Connect to server
	if c.config.UseSSL {
		c.imap, err = client.DialTLS(serverAddr, nil)
	} else {
		c.imap, err = client.Dial(serverAddr)
	}

	if err != nil {
		return fmt.Errorf("connection failed to %s: %w", serverAddr, err)
	}

	// Login with decrypted password
	if err := c.imap.Login(c.config.Username, password); err != nil {
		c.imap.Logout()

		// Log more details about the error
		fmt.Printf("Login failed for user %s: %v\n", c.config.Username, err)

		// Return wrapped error with context
		return fmt.Errorf("authentication failed: %w", err)
	}

	c.connected = true
	return nil
}

// Helper method to decrypt the password
func (c *Client) getDecryptedPassword() (string, error) {
	if c.config.EncryptedPass == "" {
		return "", fmt.Errorf("no password available")
	}
	encrypted, err := base64.StdEncoding.DecodeString(c.config.EncryptedPass)
	if err != nil {
		return "", fmt.Errorf("failed to decode password: %w", err)
	}

	decrypted, err := c.crypto.Decrypt(encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt password: %w", err)
	}

	return string(decrypted), nil
}

func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil
	}

	if err := c.imap.Logout(); err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}

	c.connected = false
	return nil
}

func (c *Client) ensureConnected() error {
	c.mu.RLock()
	connected := c.connected
	c.mu.RUnlock()

	if !connected {
		return c.Connect()
	}
	return nil
}

type FetchOptions struct {
	Folder    string
	Start     uint32
	Count     uint32
	FetchBody bool
	UseCache  bool
}

func (c *Client) FetchMessages(ctx context.Context, opts FetchOptions) ([]*models.Email, error) {
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	// Select mailbox
	mbox, err := c.imap.Select(opts.Folder, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select folder: %w", err)
	}

	// Calculate sequence numbers
	if opts.Start == 0 {
		opts.Start = 1
	}
	if opts.Count == 0 {
		opts.Count = mbox.Messages
	}

	end := opts.Start + opts.Count - 1
	if end > mbox.Messages {
		end = mbox.Messages
	}

	// Create sequence set
	seqSet := new(imap.SeqSet)
	seqSet.AddRange(opts.Start, end)

	// Prepare fetch items
	fetchItems := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid}
	if opts.FetchBody {
		fetchItems = append(fetchItems, imap.FetchBody, imap.FetchBodyStructure)
	}

	// Channel for receiving messages
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	// Start fetch operation
	go func() {
		done <- c.imap.Fetch(seqSet, fetchItems, messages)
	}()

	var emails []*models.Email
	var fetchWg sync.WaitGroup
	results := make(chan *models.Email, 10)

	// Start workers to process messages
	for i := 0; i < 5; i++ {
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			for msg := range messages {
				fmt.Println(msg)
				email, err := c.processMessage(msg, opts)
				if err != nil {
					continue
				}
				select {
				case results <- email:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Close results channel when all workers are done
	go func() {
		fetchWg.Wait()
		close(results)
	}()

	// Collect results
	for email := range results {
		emails = append(emails, email)
	}

	// Check fetch operation error
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}

	return emails, nil
}

func (c *Client) processMessage(msg *imap.Message, opts FetchOptions) (*models.Email, error) {
	// Generate cache key
	cacheKey := fmt.Sprintf("%s-%d", opts.Folder, msg.Uid)

	// Check cache first if enabled
	if opts.UseCache {
		if cached, err := c.getFromCache(cacheKey); err == nil {
			return cached, nil
		}
	}
	email := &models.Email{
		UID:       msg.Uid,
		MessageID: msg.Envelope.MessageId,
		Folder:    opts.Folder,
		Subject:   msg.Envelope.Subject,
		Date:      msg.Envelope.Date,
		Flags:     make([]string, len(msg.Flags)),
		CacheKey:  cacheKey,
	}

	// Copy flags
	for i, flag := range msg.Flags {
		email.Flags[i] = string(flag)
	}

	// Process addresses
	if len(msg.Envelope.From) > 0 {
		email.From = models.Address{
			Name:    msg.Envelope.From[0].PersonalName,
			Address: msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName,
		}
	}

	email.To = convertAddresses(msg.Envelope.To)
	email.Cc = convertAddresses(msg.Envelope.Cc)
	email.Bcc = convertAddresses(msg.Envelope.Bcc)

	// Fetch body if requested
	if opts.FetchBody {
		body, err := c.fetchBody(msg)
		fmt.Println(msg)

		if err != nil {
			return nil, err
		}
		email.Body = *body
	}

	// Cache the email
	if opts.UseCache {
		if err := c.cacheEmail(email); err != nil {
			// Log error but don't fail
			fmt.Printf("Failed to cache email: %v\n", err)
		}
	}

	return email, nil
}

func (c *Client) fetchBody(msg *imap.Message) (*models.Body, error) {
	var body models.Body

	// Get the whole message body
	section := &imap.BodySectionName{}
	r := msg.GetBody(section)
	if r == nil {
		return nil, fmt.Errorf("no message body")
	}

	// Parse the message
	mr, err := mail.CreateReader(r)
	if err != nil {
		return nil, err
	}

	// Process each part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			// Get Content-Type
			contentType, _, err := h.ContentType()
			if err != nil {
				continue
			}

			// Read the body
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, p.Body); err != nil {
				continue
			}

			switch contentType {
			case "text/plain":
				body.Text = buf.String()
			case "text/html":
				body.HTML = buf.String()
			}

		case *mail.AttachmentHeader:
			filename, err := h.Filename()
			if err != nil {
				continue
			}

			contentType, _, err := h.ContentType()
			if err != nil {
				continue
			}

			// Get attachment size
			var buf bytes.Buffer
			size, err := io.Copy(&buf, p.Body)
			if err != nil {
				continue
			}

			// Generate cache key for attachment
			cacheKey := fmt.Sprintf("attach-%d-%s", msg.Uid, filename)

			// Cache attachment
			if err := c.cache.Set(cacheKey, buf.Bytes(), true); err != nil {
				continue
			}

			body.Attached = append(body.Attached, models.AttachmentMeta{
				Filename:    filename,
				ContentType: contentType,
				Size:        size,
				CacheKey:    cacheKey,
			})
		}
	}

	return &body, nil
}

func (c *Client) getFromCache(key string) (*models.Email, error) {
	data, err := c.cache.Get(key)
	if err != nil {
		return nil, err
	}

	var email models.Email
	if err := json.Unmarshal(data, &email); err != nil {
		return nil, err
	}

	return &email, nil
}

func (c *Client) cacheEmail(email *models.Email) error {
	data, err := json.Marshal(email)
	if err != nil {
		return err
	}

	return c.cache.Set(email.CacheKey, data, true)
}

func convertAddresses(addrs []*imap.Address) []models.Address {
	result := make([]models.Address, len(addrs))
	for i, addr := range addrs {
		result[i] = models.Address{
			Name:    addr.PersonalName,
			Address: addr.MailboxName + "@" + addr.HostName,
		}
	}
	return result
}

// Additional helper methods for common operations
func (c *Client) GetFolders() ([]string, error) {
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	mailboxes := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.imap.List("", "*", mailboxes)
	}()

	var folders []string
	for m := range mailboxes {
		folders = append(folders, m.Name)
	}

	if err := <-done; err != nil {
		return nil, err
	}

	return folders, nil
}

func (c *Client) MarkMessageSeen(uid uint32, folder string) error {
	if err := c.ensureConnected(); err != nil {
		return err
	}

	if _, err := c.imap.Select(folder, false); err != nil {
		return err
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	return c.imap.Store(seqSet, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
}

func (c *Client) MoveMessage(uid uint32, fromFolder, toFolder string) error {
	if err := c.ensureConnected(); err != nil {
		return err
	}

	if _, err := c.imap.Select(fromFolder, false); err != nil {
		return err
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	return c.imap.Move(seqSet, toFolder)
}
