// handlers/api/oauth_sasl.go
//
// SASL mechanisms for OAuth2 bearer-token authentication.
//
//   - IMAP: emersion/go-imap uses an emersion/go-sasl Client. The pinned
//     go-sasl version ships OAUTHBEARER but not XOAUTH2, so XOAUTH2 is
//     implemented here.
//   - SMTP: net/smtp only ships PLAIN and CRAM-MD5, so both XOAUTH2 and
//     OAUTHBEARER smtp.Auth implementations live here.
package api

import (
	"fmt"
	"net/smtp"
	"strconv"

	"github.com/emersion/go-sasl"
)

// Xoauth2 is the SASL mechanism name for XOAUTH2.
const Xoauth2 = "XOAUTH2"

// --- IMAP (sasl.Client) ---

type xoauth2Client struct {
	username string
	token    string
}

func (a *xoauth2Client) Start() (mech string, ir []byte, err error) {
	return Xoauth2, []byte("user=" + a.username + "\x01auth=Bearer " + a.token + "\x01\x01"), nil
}

func (a *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	// A challenge is only sent when authentication fails; it carries a base64
	// JSON error. Returning an error makes go-imap cancel the exchange cleanly.
	return nil, fmt.Errorf("XOAUTH2 authentication failed: %s", string(challenge))
}

// NewXoauth2Client returns a SASL client implementing the XOAUTH2 mechanism
// for use with go-imap's Client.Authenticate.
func NewXoauth2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

// --- SMTP (smtp.Auth) ---

type smtpXoauth2 struct {
	username string
	token    string
}

func (a *smtpXoauth2) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return Xoauth2, []byte("user=" + a.username + "\x01auth=Bearer " + a.token + "\x01\x01"), nil
}

func (a *smtpXoauth2) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("xoauth2: server returned an error: %s", string(fromServer))
	}
	return nil, nil
}

// NewSMTPXoauth2 returns an smtp.Auth implementing XOAUTH2.
func NewSMTPXoauth2(username, token string) smtp.Auth {
	return &smtpXoauth2{username: username, token: token}
}

type smtpOAuthBearer struct {
	username string
	token    string
	host     string
	port     int
}

func (a *smtpOAuthBearer) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	str := "n,a=" + a.username + ","
	if a.host != "" {
		str += "\x01host=" + a.host
	}
	if a.port != 0 {
		str += "\x01port=" + strconv.Itoa(a.port)
	}
	str += "\x01auth=Bearer " + a.token + "\x01\x01"
	return sasl.OAuthBearer, []byte(str), nil
}

func (a *smtpOAuthBearer) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Acknowledge the error challenge with an empty response so the server
		// can return its final failure status.
		return []byte(""), nil
	}
	return nil, nil
}

// NewSMTPOAuthBearer returns an smtp.Auth implementing OAUTHBEARER (RFC 7628).
func NewSMTPOAuthBearer(username, token, host string, port int) smtp.Auth {
	return &smtpOAuthBearer{username: username, token: token, host: host, port: port}
}
