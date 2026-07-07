package api

import (
	"strings"
	"syscall"
	"testing"
)

// callControl exercises a screeningDialer's Control hook the way net.Dial would:
// with the concrete, already-resolved "ip:port" it is about to connect to. A nil
// RawConn is fine — the hook never touches it.
func callControl(t *testing.T, hostString, dialIPPort string) error {
	t.Helper()
	d := screeningDialer(hostString)
	if d.Control == nil {
		t.Fatal("screeningDialer produced no Control hook")
	}
	return d.Control("tcp", dialIPPort, syscall.RawConn(nil))
}

// TestConnectedAccountDial_ScreensMetadata verifies the connected-account IMAP
// dial refuses the cloud instance-metadata endpoint — the highest-value SSRF
// target — regardless of whether it arrives as the host string or as the
// resolved connect IP (DNS rebind).
func TestConnectedAccountDial_ScreensMetadata(t *testing.T) {
	// Host-string form: rejected before any dial by NewClientScreened.
	if _, err := NewClientScreened("169.254.169.254", 993, "u", "p"); err == nil {
		t.Fatal("expected metadata host string to be refused before dialing")
	}
	if _, err := NewClientScreened("metadata.google.internal", 993, "u", "p"); err == nil {
		t.Fatal("expected GCP metadata host name to be refused before dialing")
	}

	// Rebind form: a public name that resolves to the metadata IP must be refused
	// at connect time by the Control hook.
	if err := callControl(t, "imap.totally-legit.example", "169.254.169.254:993"); err == nil {
		t.Fatal("expected metadata IP to be refused at connect time")
	}
}

// TestConnectedAccountDial_ScreensRebindToPrivate verifies that a PUBLIC host
// name resolving to a loopback/private/link-local address (the classic DNS-rebind
// SSRF) is refused at connect time.
func TestConnectedAccountDial_ScreensRebindToPrivate(t *testing.T) {
	for _, ipPort := range []string{
		"127.0.0.1:993",
		"10.1.2.3:993",
		"192.168.0.5:993",
		"169.254.10.10:993", // link-local
		"0.0.0.0:993",       // unspecified
	} {
		if err := callControl(t, "imap.public-name.example", ipPort); err == nil {
			t.Fatalf("expected public host resolving to %s to be refused (rebind)", ipPort)
		}
	}
}

// TestConnectedAccountDial_AllowsExplicitPrivateHost verifies that a self-host
// operator who explicitly points a connected account at a LAN IMAP literal is NOT
// broken: when the host STRING is itself private/loopback, private dial IPs pass
// the screen. Only the metadata endpoint stays refused.
func TestConnectedAccountDial_AllowsExplicitPrivateHost(t *testing.T) {
	allowed := []struct{ host, ipPort string }{
		{"192.168.1.10", "192.168.1.10:993"},
		{"127.0.0.1", "127.0.0.1:993"},
		{"mailserver", "10.0.0.9:993"}, // single-label internal name
	}
	for _, tc := range allowed {
		if err := callControl(t, tc.host, tc.ipPort); err != nil {
			t.Fatalf("explicit private host %q -> %s should be allowed, got: %v", tc.host, tc.ipPort, err)
		}
	}

	// Even for an explicit-private host string, the metadata IP stays refused.
	if err := callControl(t, "192.168.1.10", "169.254.169.254:993"); err == nil {
		t.Fatal("metadata IP must be refused even for an explicit-private host string")
	}
}

// TestConnectedAccountDial_AllowsPublicResolution verifies a normal public IMAP
// server (public name -> public IP) passes the screen, so the feature keeps
// working for Gmail/Yahoo/etc.
func TestConnectedAccountDial_AllowsPublicResolution(t *testing.T) {
	if err := callControl(t, "imap.gmail.com", "142.250.1.109:993"); err != nil {
		t.Fatalf("public IMAP resolution should be allowed, got: %v", err)
	}
}

// TestNewClientScreened_MetadataErrorMessage keeps the refusal reason legible so
// an operator debugging a rejected connected account is not left guessing.
func TestNewClientScreened_MetadataErrorMessage(t *testing.T) {
	_, err := NewClientScreened("169.254.169.254", 993, "u", "p")
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected a metadata-refusal error, got: %v", err)
	}
}
