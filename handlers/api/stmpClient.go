package api

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net/smtp"
	"os"
	"time"
)

// SMTPClient handles email sending
type SMTPClient struct {
	server   string
	port     int
	email    string
	password string
}

// NewSMTPClient creates a new SMTP client
func NewSMTPClient(server string, port int, email, password string) *SMTPClient {
	return &SMTPClient{
		server:   server,
		port:     port,
		email:    email,
		password: password,
	}
}

// SendMail sends an email using SMTP
func (c *SMTPClient) SendMail(to, subject, body string) error {
	// Debug print
	fmt.Printf("Connecting to %s:%d as %s\n", c.server, c.port, c.email)

	// Connect to the server
	addr := fmt.Sprintf("%s:%d", c.server, c.port)
	client, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("dial failed: %v", err)
	}
	defer client.Close()

	// Send EHLO with domain from email
	domain := GetDomainFromEmail(c.email)
	if err := client.Hello(domain); err != nil {
		return fmt.Errorf("hello failed: %v", err)
	}

	// Start TLS
	tlsConfig := &tls.Config{
		ServerName:         c.server,
		InsecureSkipVerify: true,
	}
	if err = client.StartTLS(tlsConfig); err != nil {
		return fmt.Errorf("starttls failed: %v", err)
	}

	username := GetUsernameFromEmail(c.email)
	// Authenticate after TLS
	auth := smtp.PlainAuth("", username, c.password, c.server)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("auth failed: %v", err)
	}

	// Set sender
	if err = client.Mail(c.email); err != nil {
		return fmt.Errorf("mail from failed: %v", err)
	}

	// Set recipient
	if err = client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to failed: %v", err)
	}

	// Send the email body
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("data failed: %v", err)
	}

	// Get current time in RFC822 format
	now := time.Now().Format(time.RFC822Z)

	// Construct proper email headers and body
	msg := fmt.Sprintf("Date: %s\r\n"+
		"From: %s <%s>\r\n"+
		"To: %s\r\n"+
		"Subject: %s\r\n"+
		"MIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=\"utf-8\"\r\n"+
		"Message-ID: <%s@%s>\r\n"+
		"\r\n"+
		"%s",
		now,
		username,
		c.email,
		to,
		subject,
		generateMessageID(), // You'll need to implement this
		domain,
		body)

	_, err = writer.Write([]byte(msg))
	if err != nil {
		return fmt.Errorf("write failed: %v", err)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("close failed: %v", err)
	}

	return client.Quit()
}

// generateMessageID creates a unique Message-ID for the email
func generateMessageID() string {
	return fmt.Sprintf("%d.%d.%d",
		time.Now().UnixNano(),
		os.Getpid(),
		rand.Int63())
}
