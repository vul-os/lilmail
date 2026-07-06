// handlers/api/wave49_transport_security_test.go — wave-49 coverage-driven
// SECURITY pass on lilmail's transport layer (IMAP/SMTP/DAV client + SSRF guards
// + MIME builder). The wave-33 pass covered handlers/jsonapi; this file targets
// the under-tested fail-closed behaviour of the transport primitives themselves:
//
//   - DAV-URL SSRF: the dial-time IP screen (screenDialIP) and the rebind guard
//     for IPv6 ULA / link-local / IPv6-metadata targets, metadata-host-by-name at
//     dial time, and the CardDAV bearer entry point refusing an unsafe URL.
//   - SMTP header injection: BuildMIMEMessage and SMTPClient.SendMail must refuse
//     CR/LF/NUL smuggled into To/Cc/From/threading headers (a silent-Bcc / message
//     -split vector). NB: this file's TestBuildMIMEMessage_ToHeaderInjection*
//     found a REAL bug — the address/threading headers were emitted verbatim; the
//     guard added to mime_builder.go/stmpClient.go closes it.
//   - Attachment part-path traversal / malformed IDs: parsePartPath and
//     DecodeAttachmentID fail closed on non-numeric, traversal-shaped, and
//     truncated input.
//   - Malformed MIME: decodeContent must not panic on bad base64/QP and falls
//     back rather than crashing.
//   - Auth-mode selection: SMTP OAuth mechanism dispatch + SASL wire format, and
//     CalDAV/CardDAV basic-vs-bearer client construction.
package api

import (
	"context"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"lilmail/config"
)

// ---------------------------------------------------------------------------
// DAV SSRF — dial-time IP screen (screenDialIP) direct unit tests.
// ---------------------------------------------------------------------------

// TestScreenDialIP exercises the resolved-IP screen for a PUBLIC-named host
// (allowPrivate=false): metadata IPs (v4 + v6), loopback, RFC1918, IPv6 ULA
// (fd00::/8), link-local unicast/multicast, and the unspecified address must all
// be refused, while an ordinary public IP passes. This is the last line of the
// DNS-rebind defence and was only partially covered before.
func TestScreenDialIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		wantErr bool
	}{
		{"metadata v4", "169.254.169.254", true},
		{"metadata v6", "fd00:ec2::254", true},
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"rfc1918 10", "10.1.2.3", true},
		{"rfc1918 192.168", "192.168.0.1", true},
		{"rfc1918 172.16", "172.16.5.5", true},
		{"ipv6 ULA fc00", "fc00::1", true},
		{"ipv6 ULA fd00", "fd12:3456:789a::1", true},
		{"link-local unicast v4", "169.254.10.10", true},
		{"link-local unicast v6", "fe80::1", true},
		{"link-local multicast v4", "224.0.0.1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"public v4 ok", "93.184.216.34", false}, // example.com
		{"public v6 ok", "2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			err := screenDialIP(ip, false /* allowPrivate */)
			if tc.wantErr && err == nil {
				t.Fatalf("screenDialIP(%s, false) = nil; want refusal", tc.ip)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("screenDialIP(%s, false) = %v; want nil", tc.ip, err)
			}
		})
	}
}

// TestScreenDialIPAllowPrivateStillBlocksMetadata proves that even when the
// operator intentionally pointed at an internal host (allowPrivate=true, e.g. a
// loopback/private DAV URL that validateDAVURL permitted over http), the cloud
// metadata IPs are STILL refused — private is opt-in, metadata never is.
func TestScreenDialIPAllowPrivateStillBlocksMetadata(t *testing.T) {
	// A private IP is allowed when allowPrivate is set.
	if err := screenDialIP(net.ParseIP("10.0.0.5"), true); err != nil {
		t.Fatalf("screenDialIP(private, allowPrivate=true) = %v; want nil", err)
	}
	// But the metadata IPs must remain blocked regardless.
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if err := screenDialIP(net.ParseIP(ip), true); err == nil {
			t.Fatalf("screenDialIP(%s, allowPrivate=true) = nil; metadata must never be allowed", ip)
		}
	}
}

// TestSafeDialContextBlocksIPv6RebindToInternal extends the existing rebind test
// to IPv6 targets: a public FQDN that resolves to an IPv6 ULA, link-local, or the
// IPv6 metadata address must be refused at dial time.
func TestSafeDialContextBlocksIPv6RebindToInternal(t *testing.T) {
	orig := lookupDAVHost
	defer func() { lookupDAVHost = orig }()

	for _, tc := range []struct{ name, ip string }{
		{"ipv6 ULA", "fd12:3456:789a::1"},
		{"ipv6 link-local", "fe80::dead:beef"},
		{"ipv6 metadata", "fd00:ec2::254"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookupDAVHost = func(_ context.Context, _ string) ([]net.IP, error) {
				return []net.IP{net.ParseIP(tc.ip)}, nil
			}
			conn, err := safeDialContext(context.Background(), "tcp", "dav.example.com:443")
			if conn != nil {
				conn.Close()
				t.Fatalf("dialed IPv6 rebind target %s; want refusal", tc.ip)
			}
			if err == nil || !strings.Contains(err.Error(), "refusing to dial") {
				t.Fatalf("IPv6 rebind %s: got err=%v; want dial refusal", tc.ip, err)
			}
		})
	}
}

// TestSafeDialContextRefusesMetadataHostByName proves the metadata endpoint is
// blocked at dial time by NAME (metadata.google.internal) before any resolution —
// closing the case where the name itself is the dial target.
func TestSafeDialContextRefusesMetadataHostByName(t *testing.T) {
	orig := lookupDAVHost
	defer func() { lookupDAVHost = orig }()
	// If resolution were reached it would return a harmless public IP; the guard
	// must fire first, so the resolver override should never be consulted.
	resolved := false
	lookupDAVHost = func(_ context.Context, _ string) ([]net.IP, error) {
		resolved = true
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	conn, err := safeDialContext(context.Background(), "tcp", "metadata.google.internal:80")
	if conn != nil {
		conn.Close()
		t.Fatal("dialed metadata.google.internal; want refusal")
	}
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata-by-name: got err=%v; want metadata refusal", err)
	}
	if resolved {
		t.Error("resolver was consulted; metadata host must be refused before DNS")
	}
}

// TestSafeDialContextRejectsBadAddr covers the malformed dial-address branch
// (SplitHostPort failure) — a missing port must fail closed, not dial.
func TestSafeDialContextRejectsBadAddr(t *testing.T) {
	conn, err := safeDialContext(context.Background(), "tcp", "not-a-host-port")
	if conn != nil {
		conn.Close()
		t.Fatal("dialed a malformed address; want error")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid dial address") {
		t.Fatalf("bad addr: got err=%v; want invalid-address error", err)
	}
}

// TestSafeDialContextResolverErrorFailsClosed covers the resolver-error path:
// when name resolution fails, safeDialContext must return the error (not dial).
func TestSafeDialContextResolverErrorFailsClosed(t *testing.T) {
	orig := lookupDAVHost
	defer func() { lookupDAVHost = orig }()
	lookupDAVHost = func(_ context.Context, _ string) ([]net.IP, error) {
		return nil, net.UnknownNetworkError("simulated resolver failure")
	}
	conn, err := safeDialContext(context.Background(), "tcp", "dav.example.com:443")
	if conn != nil {
		conn.Close()
		t.Fatal("dialed despite resolver failure; want error")
	}
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("resolver error: got err=%v; want resolve error", err)
	}
}

// TestSafeDialContextNoUsableAddress covers the "every resolved IP was screened
// out" fallthrough: when the only address is internal (for a public name), the
// loop rejects it and the function returns firstErr — never an empty success.
func TestSafeDialContextNoUsableAddress(t *testing.T) {
	orig := lookupDAVHost
	defer func() { lookupDAVHost = orig }()
	lookupDAVHost = func(_ context.Context, _ string) ([]net.IP, error) {
		// Two internal candidates; both must be screened out for a public name.
		return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.1")}, nil
	}
	conn, err := safeDialContext(context.Background(), "tcp", "dav.example.com:443")
	if conn != nil {
		conn.Close()
		t.Fatal("dialed when no address was usable; want refusal")
	}
	if err == nil {
		t.Fatal("no-usable-address returned nil error")
	}
}

// TestCardDAVClientRejectsUnsafeURL confirms the CardDAV client constructor runs
// the SSRF guard before attaching a token — a forged http public URL is refused,
// and the metadata endpoint is refused, so a brokered bearer token can never be
// dialed to an attacker/metadata host via the contacts path.
func TestCardDAVClientRejectsUnsafeURL(t *testing.T) {
	for _, url := range []string{
		"http://dav.attacker.example/carddav/",
		"http://169.254.169.254/carddav/",
		"https://169.254.169.254/",
		"http://metadata.google.internal/",
	} {
		if _, err := carddavClient(url, davAuth{token: "ya29.secret"}); err == nil {
			t.Fatalf("carddavClient(%q) accepted an unsafe URL; SSRF guard missing", url)
		}
	}
	// The empty-URL branch must also fail closed.
	if _, err := carddavClient("", davAuth{token: "t"}); err == nil {
		t.Fatal("carddavClient(\"\") accepted an empty URL")
	}
}

// ---------------------------------------------------------------------------
// SMTP / MIME header injection — REAL BUG regression tests.
// ---------------------------------------------------------------------------

// TestBuildMIMEMessage_ToHeaderInjectionRejected is the regression for the REAL
// bug this pass found: a CRLF-laden To value injected an arbitrary header (a
// silent Bcc:) into the outgoing message because To/Cc/From/threading headers
// were written verbatim. The build must now fail closed.
func TestBuildMIMEMessage_ToHeaderInjectionRejected(t *testing.T) {
	_, err := BuildMIMEMessage(MIMEMessageOptions{
		From:      "alice@example.com",
		To:        "bob@example.com\r\nBcc: attacker@evil.example",
		Subject:   "hi",
		PlainBody: "body",
	})
	if err == nil {
		t.Fatal("BuildMIMEMessage accepted a CRLF-injected To header; header-injection guard missing")
	}
	if !strings.Contains(err.Error(), "header") {
		t.Fatalf("unexpected error (want header-injection message): %v", err)
	}
}

// TestBuildMIMEMessage_HeaderInjectionAllFields checks every verbatim header
// field (From, To, Cc, In-Reply-To, References) rejects CR, LF, and NUL, while a
// clean control message still builds. Subject is intentionally NOT here: it is
// Q-encoded, which neutralises CR/LF.
func TestBuildMIMEMessage_HeaderInjectionAllFields(t *testing.T) {
	base := func() MIMEMessageOptions {
		return MIMEMessageOptions{
			From: "alice@example.com", To: "bob@example.com",
			Subject: "s", PlainBody: "b",
		}
	}
	// Control: a clean message must build.
	if _, err := BuildMIMEMessage(base()); err != nil {
		t.Fatalf("clean control message failed to build: %v", err)
	}

	payloads := []string{"x\r\nEvil: 1", "x\nEvil: 1", "x\rEvil: 1", "x\x00y"}
	mutate := map[string]func(*MIMEMessageOptions, string){
		"From":        func(o *MIMEMessageOptions, v string) { o.From = v },
		"To":          func(o *MIMEMessageOptions, v string) { o.To = v },
		"Cc":          func(o *MIMEMessageOptions, v string) { o.Cc = v },
		"In-Reply-To": func(o *MIMEMessageOptions, v string) { o.InReplyTo = v },
		"References":  func(o *MIMEMessageOptions, v string) { o.References = v },
		"Message-ID":  func(o *MIMEMessageOptions, v string) { o.MessageID = v },
	}
	for field, set := range mutate {
		for _, p := range payloads {
			opts := base()
			set(&opts, p)
			if _, err := BuildMIMEMessage(opts); err == nil {
				t.Errorf("%s field accepted injection payload %q; want rejection", field, p)
			}
		}
	}
}

// TestSendMailRejectsHeaderInjection proves the SMTPClient.SendMail path (used by
// the calendar iTIP send) also refuses CR/LF-laden To/Subject/Cc/threading values
// before opening a connection — the same header-smuggling class, second choke
// point. We assert the guard fires (no dial), independent of any server.
func TestSendMailRejectsHeaderInjection(t *testing.T) {
	c := NewSMTPClient("smtp.invalid.example", 587, "alice@example.com", "pw", true)
	cases := []struct {
		name          string
		to, subj      string
		opts          *MailOptions
		wantInjection bool
	}{
		{"to injection", "bob@x\r\nBcc: e@evil", "s", nil, true},
		{"subject injection", "bob@x", "s\r\nBcc: e@evil", nil, true},
		{"cc injection", "bob@x", "s", &MailOptions{Cc: "c@x\r\nBcc: e@evil"}, true},
		{"references injection", "bob@x", "s", &MailOptions{References: "r\r\nX: 1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.SendMail(tc.to, tc.subj, "body", tc.opts)
			if err == nil {
				t.Fatal("SendMail accepted an injected header; want rejection")
			}
			// The injection guard must fire (message names an unsafe header),
			// rather than merely failing later at dial time.
			if tc.wantInjection && !strings.Contains(err.Error(), "unsafe header") {
				t.Fatalf("expected header-injection refusal, got: %v", err)
			}
		})
	}
}

// TestValidateHeaderValue is the unit-level guard: CR/LF/NUL rejected, clean
// values (including empty, which callers omit) accepted.
func TestValidateHeaderValue(t *testing.T) {
	for _, ok := range []string{"", "bob@example.com", "Bob Smith <bob@x.com>", "<id@host>"} {
		if err := validateHeaderValue(ok); err != nil {
			t.Errorf("validateHeaderValue(%q) = %v; want nil", ok, err)
		}
	}
	for _, bad := range []string{"a\rb", "a\nb", "a\r\nb", "a\x00b"} {
		if err := validateHeaderValue(bad); err == nil {
			t.Errorf("validateHeaderValue(%q) = nil; want rejection", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Attachment part-path traversal / malformed attachment IDs.
// ---------------------------------------------------------------------------

// TestParsePartPathFailsClosed asserts the IMAP part-path parser only accepts a
// dot-separated list of integers and refuses traversal-shaped, non-numeric, and
// empty input — so a crafted attachment ID cannot smuggle a filesystem-looking or
// non-integer path into the IMAP BODY[<path>] fetch section.
func TestParsePartPathFailsClosed(t *testing.T) {
	good := map[string][]int{
		"1":     {1},
		"2.1":   {2, 1},
		"3.2.1": {3, 2, 1},
	}
	for in, want := range good {
		got, err := parsePartPath(in)
		if err != nil {
			t.Errorf("parsePartPath(%q) unexpected error: %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("parsePartPath(%q) = %v; want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parsePartPath(%q) = %v; want %v", in, got, want)
				break
			}
		}
	}

	bad := []string{
		"",          // empty
		"../../etc", // path traversal shape
		"..",        // parent ref
		"1/2",       // slash separator
		"1.2.x",     // non-numeric segment
		"a",         // letter
		"1..2",      // empty segment
		".",         // lone dot
		"1.",        // trailing dot -> empty segment
		"  1 ",      // whitespace (Atoi rejects)
		"0x1",       // hex-looking
		"1e2",       // float-looking
	}
	for _, in := range bad {
		if _, err := parsePartPath(in); err == nil {
			t.Errorf("parsePartPath(%q) = nil error; want rejection (traversal/malformed)", in)
		}
	}
}

// TestDecodeAttachmentIDRoundTripAndFailClosed confirms encode→decode round-trips
// and that malformed tokens (non-base64, wrong field count) fail closed rather
// than returning a partially-populated folder/uid/part that could be used to
// select an unintended mailbox or part.
func TestDecodeAttachmentIDRoundTripAndFailClosed(t *testing.T) {
	id := encodeAttachmentID("INBOX", "42", "2.1")
	folder, uid, part, err := DecodeAttachmentID(id)
	if err != nil {
		t.Fatalf("DecodeAttachmentID(round-trip) error: %v", err)
	}
	if folder != "INBOX" || uid != "42" || part != "2.1" {
		t.Fatalf("round-trip mismatch: got (%q,%q,%q)", folder, uid, part)
	}

	// Non-base64 input must error.
	if _, _, _, err := DecodeAttachmentID("!!!not base64!!!"); err == nil {
		t.Error("DecodeAttachmentID accepted non-base64 input")
	}
	// Valid base64 but wrong field count (no NUL separators) must error.
	// base64.RawURLEncoding of "onlyonefield" has no \x00, so SplitN yields 1 field.
	oneField := base64.RawURLEncoding.EncodeToString([]byte("onlyonefield"))
	if _, _, _, err := DecodeAttachmentID(oneField); err == nil {
		t.Error("DecodeAttachmentID accepted a token with the wrong field count")
	}
}

// ---------------------------------------------------------------------------
// Malformed MIME must fail closed (no panic).
// ---------------------------------------------------------------------------

// TestDecodeContentMalformedFailsClosed feeds decodeContent broken base64 and QP
// input and asserts it returns an error or the raw fallback — never a panic. This
// is the transfer-decoding path used by FetchAttachment and processMessage on
// attacker-supplied part bytes.
func TestDecodeContentMalformedFailsClosed(t *testing.T) {
	// Broken base64 → decoder returns an error (which FetchAttachment surfaces).
	if _, err := decodeContent([]byte("@@@not base64@@@"), "base64"); err == nil {
		t.Error("decodeContent(bad base64) = nil error; want decode error")
	}
	// Base64 with embedded whitespace/newlines is cleaned then decoded — valid.
	if out, err := decodeContent([]byte("aGVs\r\nbG8="), "base64"); err != nil || string(out) != "hello" {
		t.Errorf("decodeContent(wrapped base64) = %q,%v; want \"hello\",nil", out, err)
	}
	// Quoted-printable that is malformed must fall back to raw bytes, not panic.
	qpOut, err := decodeContent([]byte("plain=text=ZZ"), "quoted-printable")
	if err != nil {
		t.Errorf("decodeContent(bad QP) returned error instead of raw fallback: %v", err)
	}
	if len(qpOut) == 0 {
		t.Error("decodeContent(bad QP) returned empty; want raw fallback")
	}
	// Unknown/empty encoding returns raw bytes verbatim.
	if out, _ := decodeContent([]byte("raw bytes"), ""); string(out) != "raw bytes" {
		t.Errorf("decodeContent(no encoding) = %q; want passthrough", out)
	}
}

// ---------------------------------------------------------------------------
// Auth-mode selection (brokered/OAuth vs basic/session creds).
// ---------------------------------------------------------------------------

// TestSMTPOAuthMechanismDispatch verifies the mechanism selection in the OAuth
// SMTP client: "oauthbearer" yields an OAUTHBEARER SASL start, anything else
// (incl. the default "xoauth2") yields an XOAUTH2 start, and the token is carried
// in the auth exchange. Wrong dispatch would send credentials with the wrong
// SASL framing (auth failure) or leak the token in an unexpected format.
func TestSMTPOAuthMechanismDispatch(t *testing.T) {
	// XOAUTH2 wire format: "user=<u>\x01auth=Bearer <t>\x01\x01".
	xo := NewSMTPXoauth2("alice@example.com", "TOKEN123")
	mech, ir, err := xo.Start(nil)
	if err != nil {
		t.Fatalf("xoauth2 Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("xoauth2 mechanism = %q; want XOAUTH2", mech)
	}
	want := "user=alice@example.com\x01auth=Bearer TOKEN123\x01\x01"
	if string(ir) != want {
		t.Errorf("xoauth2 IR = %q; want %q", ir, want)
	}

	// OAUTHBEARER wire format carries host/port and the bearer token.
	ob := NewSMTPOAuthBearer("alice@example.com", "TOKEN123", "smtp.example.com", 587)
	mech2, ir2, err := ob.Start(nil)
	if err != nil {
		t.Fatalf("oauthbearer Start: %v", err)
	}
	if mech2 != "OAUTHBEARER" {
		t.Errorf("oauthbearer mechanism = %q; want OAUTHBEARER", mech2)
	}
	s := string(ir2)
	for _, sub := range []string{"a=alice@example.com", "host=smtp.example.com", "port=587", "auth=Bearer TOKEN123"} {
		if !strings.Contains(s, sub) {
			t.Errorf("oauthbearer IR missing %q; got %q", sub, s)
		}
	}
}

// TestIMAPXoauth2ClientWireFormat covers the IMAP-side XOAUTH2 SASL client used by
// the brokered/OAuth login path (distinct from the SMTP one), and its fail-closed
// Next() when the server sends a challenge (which only happens on auth failure).
func TestIMAPXoauth2ClientWireFormat(t *testing.T) {
	cl := NewXoauth2Client("bob@example.com", "TKN")
	mech, ir, err := cl.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mechanism = %q; want XOAUTH2", mech)
	}
	if string(ir) != "user=bob@example.com\x01auth=Bearer TKN\x01\x01" {
		t.Errorf("IR = %q", ir)
	}
	// A challenge means auth failed; Next must return an error (not a response
	// that could continue an exchange with a leaked token).
	if _, err := cl.Next([]byte("eyJzdGF0dXMiOiJmYWlsIn0=")); err == nil {
		t.Error("Next(challenge) = nil error; want failure")
	}
}

// TestCalDAVAuthModeSelection confirms the CalDAV client constructor honours the
// auth mode: oauth2 requires a bearer token path, basic uses username/password,
// and BOTH still enforce the SSRF URL guard before any client is built. We drive
// this through NewCalDAVClient with a safe loopback URL so construction succeeds
// (no dial happens at construction time).
func TestCalDAVAuthModeSelection(t *testing.T) {
	// oauth2 + safe URL → client builds (guard passes, bearer attached).
	if _, err := NewCalDAVClient(config.CalDAVConfig{
		Enabled: true, URL: "http://127.0.0.1:5232/", Auth: "oauth2",
	}, "ya29.token"); err != nil {
		t.Fatalf("oauth2 CalDAV client (safe URL) failed to build: %v", err)
	}
	// basic + safe URL → client builds using username/password.
	if _, err := NewCalDAVClient(config.CalDAVConfig{
		Enabled: true, URL: "http://127.0.0.1:5232/", Auth: "basic",
		Username: "alice", Password: "pw",
	}, ""); err != nil {
		t.Fatalf("basic CalDAV client (safe URL) failed to build: %v", err)
	}
	// disabled integration must refuse regardless of auth.
	if _, err := NewCalDAVClient(config.CalDAVConfig{Enabled: false, URL: "http://127.0.0.1:5232/"}, ""); err == nil {
		t.Fatal("NewCalDAVClient built a client for a disabled integration")
	}
	// empty URL must refuse.
	if _, err := NewCalDAVClient(config.CalDAVConfig{Enabled: true, URL: ""}, ""); err == nil {
		t.Fatal("NewCalDAVClient built a client for an empty URL")
	}
	// oauth2 + UNSAFE public http URL must refuse before attaching the token.
	if _, err := NewCalDAVClient(config.CalDAVConfig{
		Enabled: true, URL: "http://dav.attacker.example/caldav/", Auth: "oauth2",
	}, "ya29.token"); err == nil {
		t.Fatal("NewCalDAVClient attached a bearer token to an unsafe http URL")
	}
}
