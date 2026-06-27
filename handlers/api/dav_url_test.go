package api

import (
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
