package api

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// SMTPClient handles email sending
type SMTPClient struct {
	server             string
	port               int
	email              string
	password           string
	useOAuth           bool
	token              string
	mechanism          string // "xoauth2" or "oauthbearer"
	insecureSkipVerify bool   // if true, skip TLS certificate verification
	useStartTLS        bool   // true = STARTTLS (port 587); false = implicit TLS (port 465)
}

// NewSMTPClient creates a new SMTP client using password authentication.
// useStartTLS controls the connection mode: true → plaintext + STARTTLS
// (port 587), false → implicit TLS from the start (port 465).
func NewSMTPClient(server string, port int, email, password string, useStartTLS bool) *SMTPClient {
	return &SMTPClient{
		server:      server,
		port:        port,
		email:       email,
		password:    password,
		useStartTLS: useStartTLS,
	}
}

// NewSMTPClientOAuth creates a new SMTP client authenticated with an OAuth2
// access token (XOAUTH2 or OAUTHBEARER).
func NewSMTPClientOAuth(server string, port int, email, token, mechanism string, useStartTLS bool) *SMTPClient {
	return &SMTPClient{
		server:      server,
		port:        port,
		email:       email,
		token:       token,
		mechanism:   mechanism,
		useOAuth:    true,
		useStartTLS: useStartTLS,
	}
}

// SetInsecureSkipVerify controls whether TLS certificate verification is skipped.
// Use only for self-signed or development servers; default is false (verify certs).
func (c *SMTPClient) SetInsecureSkipVerify(skip bool) {
	c.insecureSkipVerify = skip
}

// SMTPTransport is a snapshot of an SMTPClient's connection parameters, including
// the authentication secret (password or OAuth token). It lets a caller persist
// everything needed to reconnect + send later (e.g. a scheduled send) WITHOUT
// duplicating the credential-derivation logic in the handler layer. The secret is
// sensitive: callers MUST store it encrypted at rest, never in plaintext.
type SMTPTransport struct {
	Server       string
	Port         int
	Email        string
	UseOAuth     bool
	Mechanism    string
	UseSTARTTLS  bool
	InsecureSkip bool
	// Secret is the password (plain auth) or the OAuth access token (oauth auth).
	Secret string
}

// Transport returns a snapshot of this client's connection parameters + secret,
// for callers that need to persist a delayed send. Reusing this keeps the
// broker/session/oauth credential logic in ONE place (whoever built the client).
func (c *SMTPClient) Transport() SMTPTransport {
	secret := c.password
	if c.useOAuth {
		secret = c.token
	}
	return SMTPTransport{
		Server:       c.server,
		Port:         c.port,
		Email:        c.email,
		UseOAuth:     c.useOAuth,
		Mechanism:    c.mechanism,
		UseSTARTTLS:  c.useStartTLS,
		InsecureSkip: c.insecureSkipVerify,
		Secret:       secret,
	}
}

// MailOptions carries optional RFC 2822 header fields for a message.
type MailOptions struct {
	// Cc is a comma-separated list of CC recipients.
	Cc string
	// Bcc is a comma-separated list of BCC recipients (added as RCPT TO, not in headers).
	Bcc string
	// InReplyTo is the Message-ID of the message being replied to.
	InReplyTo string
	// References is the full References header value for threading.
	References string
}

// SendMail sends an email using SMTP.  Extra recipients and threading headers
// can be provided via opts (pass nil or &MailOptions{} for a plain send).
func (c *SMTPClient) SendMail(to, subject, body string, opts *MailOptions) error {
	if opts == nil {
		opts = &MailOptions{}
	}

	// Header-injection guard for the values written verbatim into the DATA header
	// block below. A CR/LF/NUL in any of them would terminate the header line and
	// allow header smuggling (e.g. an injected Bcc:) or message splitting. Fail
	// closed before opening the connection.
	for _, v := range []string{to, subject, opts.Cc, opts.InReplyTo, opts.References} {
		if err := validateHeaderValue(v); err != nil {
			return fmt.Errorf("smtp: refusing to send message with unsafe header: %w", err)
		}
	}

	addr := fmt.Sprintf("%s:%d", c.server, c.port)
	tlsCfg := &tls.Config{
		ServerName:         c.server,
		InsecureSkipVerify: c.insecureSkipVerify, //nolint:gosec // operator-controlled
	}

	var client *smtp.Client
	var err error

	if c.useStartTLS {
		// Plain TCP → STARTTLS upgrade.
		client, err = smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("dial failed: %v", err)
		}
		domain := GetDomainFromEmail(c.email)
		if err := client.Hello(domain); err != nil {
			client.Close()
			return fmt.Errorf("hello failed: %v", err)
		}
		if err = client.StartTLS(tlsCfg); err != nil {
			client.Close()
			return fmt.Errorf("starttls failed: %v", err)
		}
	} else {
		// Implicit TLS (port 465).
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("tls dial failed: %v", err)
		}
		host, _, _ := net.SplitHostPort(addr)
		client, err = smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return fmt.Errorf("smtp client init failed: %v", err)
		}
	}
	defer client.Close()

	domain := GetDomainFromEmail(c.email)
	username := GetUsernameFromEmail(c.email)

	// Authenticate.
	var auth smtp.Auth
	if c.useOAuth {
		switch strings.ToLower(c.mechanism) {
		case "oauthbearer":
			auth = NewSMTPOAuthBearer(c.email, c.token, c.server, c.port)
		default:
			auth = NewSMTPXoauth2(c.email, c.token)
		}
	} else {
		auth = smtp.PlainAuth("", username, c.password, c.server)
	}
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("auth failed: %v", err)
	}

	// Set sender.
	if err = client.Mail(c.email); err != nil {
		return fmt.Errorf("mail from failed: %v", err)
	}

	// Primary recipients.
	for _, rcpt := range splitAddresses(to) {
		if err = client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt to %q failed: %v", rcpt, err)
		}
	}
	// CC recipients (visible in headers).
	for _, rcpt := range splitAddresses(opts.Cc) {
		if err = client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt cc %q failed: %v", rcpt, err)
		}
	}
	// BCC recipients (RCPT TO only — not added to message headers).
	for _, rcpt := range splitAddresses(opts.Bcc) {
		if err = client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt bcc %q failed: %v", rcpt, err)
		}
	}

	// Write DATA section.
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("data failed: %v", err)
	}

	now := time.Now().Format(time.RFC822Z)
	msgID := fmt.Sprintf("<%s@%s>", generateMessageID(), domain)

	var hdr strings.Builder
	hdr.WriteString("Date: " + now + "\r\n")
	hdr.WriteString("From: " + username + " <" + c.email + ">\r\n")
	hdr.WriteString("To: " + to + "\r\n")
	if opts.Cc != "" {
		hdr.WriteString("Cc: " + opts.Cc + "\r\n")
	}
	hdr.WriteString("Subject: " + subject + "\r\n")
	hdr.WriteString("MIME-Version: 1.0\r\n")
	hdr.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	hdr.WriteString("Message-ID: " + msgID + "\r\n")
	if opts.InReplyTo != "" {
		hdr.WriteString("In-Reply-To: " + opts.InReplyTo + "\r\n")
	}
	if opts.References != "" {
		hdr.WriteString("References: " + opts.References + "\r\n")
	}
	hdr.WriteString("\r\n")
	hdr.WriteString(body)

	if _, err = writer.Write([]byte(hdr.String())); err != nil {
		return fmt.Errorf("write failed: %v", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("close failed: %v", err)
	}
	return client.Quit()
}

// SendRawMessage sends a pre-built RFC 2822 message via SMTP. The caller is
// responsible for constructing the full message including all headers and body.
// allRcpts is the union of To, CC, and BCC addresses to use as RCPT TO.
func (c *SMTPClient) SendRawMessage(allRcpts []string, rawMessage []byte) error {
	addr := fmt.Sprintf("%s:%d", c.server, c.port)
	tlsCfg := &tls.Config{
		ServerName:         c.server,
		InsecureSkipVerify: c.insecureSkipVerify, //nolint:gosec // operator-controlled
	}

	var smtpClient *smtp.Client
	var err error

	if c.useStartTLS {
		smtpClient, err = smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("dial failed: %v", err)
		}
		domain := GetDomainFromEmail(c.email)
		if err := smtpClient.Hello(domain); err != nil {
			smtpClient.Close()
			return fmt.Errorf("hello failed: %v", err)
		}
		if err = smtpClient.StartTLS(tlsCfg); err != nil {
			smtpClient.Close()
			return fmt.Errorf("starttls failed: %v", err)
		}
	} else {
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("tls dial failed: %v", err)
		}
		host, _, _ := net.SplitHostPort(addr)
		smtpClient, err = smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return fmt.Errorf("smtp client init failed: %v", err)
		}
	}
	defer smtpClient.Close()

	username := GetUsernameFromEmail(c.email)
	var auth smtp.Auth
	if c.useOAuth {
		switch strings.ToLower(c.mechanism) {
		case "oauthbearer":
			auth = NewSMTPOAuthBearer(c.email, c.token, c.server, c.port)
		default:
			auth = NewSMTPXoauth2(c.email, c.token)
		}
	} else {
		auth = smtp.PlainAuth("", username, c.password, c.server)
	}
	if err = smtpClient.Auth(auth); err != nil {
		return fmt.Errorf("auth failed: %v", err)
	}

	if err = smtpClient.Mail(c.email); err != nil {
		return fmt.Errorf("mail from failed: %v", err)
	}
	for _, rcpt := range allRcpts {
		if err = smtpClient.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt to %q failed: %v", rcpt, err)
		}
	}

	writer, err := smtpClient.Data()
	if err != nil {
		return fmt.Errorf("data failed: %v", err)
	}
	if _, err = writer.Write(rawMessage); err != nil {
		return fmt.Errorf("write failed: %v", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("close failed: %v", err)
	}
	return smtpClient.Quit()
}

// splitAddresses splits a comma-separated address list into individual entries,
// trimming whitespace and skipping empty strings.
func splitAddresses(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			result = append(result, a)
		}
	}
	return result
}

// generateMessageID creates a unique Message-ID for the email
func generateMessageID() string {
	return fmt.Sprintf("%d.%d.%d",
		time.Now().UnixNano(),
		os.Getpid(),
		rand.Int63())
}
