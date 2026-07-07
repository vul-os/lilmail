// handlers/api/dial_screen.go — SSRF screening for IMAP dials whose server host
// is USER-supplied rather than operator/broker-supplied.
//
// WHY THIS EXISTS: the connected-accounts / unified-inbox feature
// (handlers/jsonapi/accounts.go) lets an AUTHENTICATED end user register an
// additional mailbox by IMAP host/port/username/password. In a CP-brokered cloud
// deployment lilmail runs inside Vulos's own network, so an unscreened dial to a
// caller-chosen host turns lilmail into an SSRF probe of the internal network and
// the cloud instance-metadata endpoint — exactly the risk the brokered-DAV client
// already guards (see dav_url.go). The broker/session IMAP hosts are trusted
// (validated secret / operator config) and keep the plain DialTLS path; only the
// user-supplied connected-account host is routed through the screened dial here.
//
// The screening mirrors the DAV guard EXACTLY (isMetadataHost / screenDialIP):
// cloud-metadata endpoints are refused always, and loopback/private/link-local
// IPs are refused UNLESS the target host STRING is itself local/private (a
// self-host operator explicitly pointing at a LAN IMAP server — a legitimate
// configuration). Screening the POST-RESOLUTION connect IP via net.Dialer.Control
// closes the validate→dial DNS-rebind TOCTOU: a public name that resolves to an
// internal address is rejected at connect time, not merely at string-parse time.
package api

import (
	"fmt"
	"log"
	"net"
	"syscall"
	"time"

	"lilmail/storage"

	"github.com/emersion/go-imap/client"
)

// screenedDialTimeout bounds a screened connect. Kept modest so a hostile host
// (or an internal address that silently drops SYNs) cannot stall the caller.
const screenedDialTimeout = 20 * time.Second

// screeningDialer returns a *net.Dialer whose Control hook screens the ACTUAL
// resolved connect IP before the socket connects. `host` is the target host
// string; when it is itself local/private the operator has explicitly opted into
// an internal target, so private dial IPs are allowed (only cloud-metadata IPs
// are refused). For a public host string, any resolution to a loopback/private/
// link-local IP is refused as a DNS-rebind attempt.
func screeningDialer(host string) *net.Dialer {
	allowPrivate := storage.HostIsLocalOrPrivate(host)
	return &net.Dialer{
		Timeout: screenedDialTimeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				h = address
			}
			ip := net.ParseIP(h)
			if ip == nil {
				return fmt.Errorf("imap: refusing unresolved dial address %q", address)
			}
			return screenDialIP(ip, allowPrivate)
		},
	}
}

// NewClientScreened is NewClient with SSRF screening of the (user-supplied) server
// host. It is used for CONNECTED accounts, whose IMAP coordinates arrive from an
// authenticated end user rather than a trusted broker/operator. The host-string
// metadata check rejects an obvious metadata target up front; the dialer's Control
// hook then re-screens the resolved IP so a rebind cannot slip through.
func NewClientScreened(server string, port int, email, password string) (*Client, error) {
	if isMetadataHost(server) {
		return nil, fmt.Errorf("connection error: refusing to dial cloud metadata endpoint %q", server)
	}
	c, err := client.DialWithDialerTLS(screeningDialer(server), fmt.Sprintf("%s:%d", server, port), nil)
	if err != nil {
		log.Printf("DialTLS(screened) %s:%d connection err: %v", server, port, err)
		return nil, fmt.Errorf("connection error: %v", err)
	}
	if err := c.Login(email, password); err != nil {
		c.Logout()
		log.Printf("IMAP Login %s/xxx login err: %v", email, err)
		return nil, fmt.Errorf("login error: %v", err)
	}
	return &Client{client: c, username: email}, nil
}
