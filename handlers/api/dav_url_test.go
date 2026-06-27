package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lilmail/config"
)

// TestValidateDAVURL asserts the SSRF / token-exfil guard: https is required for
// public hosts, http is tolerated only for loopback/private hosts, and cloud
// metadata endpoints are rejected outright.
func TestValidateDAVURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https public ok", "https://dav.example.com/caldav/", false},
		{"http public rejected", "http://dav.example.com/caldav/", true},
		{"http loopback ok", "http://127.0.0.1:5232/", false},
		{"http localhost ok", "http://localhost:5232/", false},
		{"http private ok", "http://10.0.0.5/dav/", false},
		{"http single-label ok", "http://radicale/dav/", false},
		{"https metadata rejected", "https://169.254.169.254/", true},
		{"http metadata rejected", "http://169.254.169.254/latest/meta-data/", true},
		{"gcp metadata name rejected", "http://metadata.google.internal/", true},
		{"ipv6 metadata rejected", "http://[fd00:ec2::254]/", true},
		{"empty rejected", "", true},
		{"no scheme rejected", "dav.example.com/caldav/", true},
		{"junk rejected", "://nonsense", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDAVURL(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("validateDAVURL(%q) = nil; want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDAVURL(%q) = %v; want nil", tc.url, err)
			}
		})
	}
}

// TestNewCalDAVClientRejectsUnsafeURL confirms the guard runs inside the client
// constructor (so the CP-brokered dial seam can never attach a bearer token to a
// forged endpoint), before any network dial.
func TestNewCalDAVClientRejectsUnsafeURL(t *testing.T) {
	cfg := config.CalDAVConfig{Enabled: true, URL: "http://dav.attacker.example/caldav/", Auth: "oauth2"}
	_, err := NewCalDAVClient(cfg, "ya29.secret-access-token")
	if err == nil {
		t.Fatal("NewCalDAVClient accepted an http:// public URL; SSRF/token-exfil guard missing")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error (want transport-safety message): %v", err)
	}
}

// TestCardDAVContactsBearerRejectsUnsafeURL confirms the bearer CardDAV path
// refuses an unsafe URL (returning nil rather than dialing with the token).
func TestCardDAVContactsBearerRejectsUnsafeURL(t *testing.T) {
	got := CardDAVContactsBearer("http://dav.attacker.example/carddav/", "ya29.secret-access-token", "al", 10)
	if got != nil {
		t.Fatalf("CardDAVContactsBearer dialed an unsafe URL; want nil, got %v", got)
	}
}

// TestSafeDAVHTTPClientBlocksRedirectToInternal proves the CheckRedirect guard:
// a server that validates fine on the first hop cannot 302-bounce the client to a
// cloud-metadata / internal host. The redirect target is never dialed.
func TestSafeDAVHTTPClientBlocksRedirectToInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := safeDAVHTTPClient().Get(srv.URL)
	if err == nil {
		t.Fatal("client followed a redirect to a metadata host; want error")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("unexpected error (want redirect refusal): %v", err)
	}
}

// TestSafeDialContextBlocksRebindToInternal proves the per-dial IP screen: a
// public-looking hostname that RESOLVES to an internal/metadata IP is refused at
// dial time (closing the validate→dial DNS-rebind window). lookupDAVHost is
// overridden to simulate the rebind; the offending IP is screened before any dial.
func TestSafeDialContextBlocksRebindToInternal(t *testing.T) {
	orig := lookupDAVHost
	defer func() { lookupDAVHost = orig }()

	cases := []struct {
		name string
		ip   string
	}{
		{"metadata", "169.254.169.254"},
		{"loopback", "127.0.0.1"},
		{"private", "10.0.0.5"},
		{"link-local", "169.254.10.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookupDAVHost = func(_ context.Context, _ string) ([]net.IP, error) {
				return []net.IP{net.ParseIP(tc.ip)}, nil
			}
			// "dav.example.com" is a public FQDN: validateDAVURL would accept it, so
			// only the dial-time screen stops the rebind.
			conn, err := safeDialContext(context.Background(), "tcp", "dav.example.com:443")
			if conn != nil {
				conn.Close()
				t.Fatalf("dialed rebind target %s; want refusal", tc.ip)
			}
			if err == nil {
				t.Fatalf("dial of rebind target %s returned no error", tc.ip)
			}
			if !strings.Contains(err.Error(), "refusing to dial") {
				t.Fatalf("unexpected error for %s (want dial refusal): %v", tc.ip, err)
			}
		})
	}
}

// TestSafeDialContextAllowsIntentionalInternal confirms operator-intended internal
// targets still dial: when the URL host is itself loopback/private, a private dial
// IP is permitted (only metadata IPs are refused unconditionally).
func TestSafeDialContextAllowsIntentionalInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:<port>; the host is loopback, so allowPrivate is
	// true and the dial must succeed through the hardened client.
	resp, err := safeDAVHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("hardened client refused a legitimate loopback dial: %v", err)
	}
	resp.Body.Close()
}
