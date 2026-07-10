package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempConfig writes body to a temp config.toml and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// minimalIMAP is the smallest valid body LoadConfig needs (SSL is disabled so
// no certificate validation runs).
const minimalIMAP = `
[imap]
server = "imap.example.com"
port = 993
`

func TestAllowFullEmailUsername_AuthSectionWins(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "default is full email (true)",
			body: minimalIMAP,
			want: true,
		},
		{
			name: "auth=false sends bare handle",
			body: minimalIMAP + "\n[auth]\nallow_full_email_username = false\n",
			want: false,
		},
		{
			name: "auth=true sends full email",
			body: minimalIMAP + "\n[auth]\nallow_full_email_username = true\n",
			want: true,
		},
		{
			name: "legacy server.username_is_email=false honoured when auth absent",
			body: "[server]\nusername_is_email = false\n" + minimalIMAP,
			want: false,
		},
		{
			name: "auth overrides legacy server key",
			body: "[server]\nusername_is_email = false\n" + minimalIMAP + "\n[auth]\nallow_full_email_username = true\n",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeTempConfig(t, tc.body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			// Server.UsernameIsEmail is the single source of truth all auth
			// paths read; it must reflect the reconciled value.
			if cfg.Server.UsernameIsEmail != tc.want {
				t.Errorf("Server.UsernameIsEmail = %v; want %v", cfg.Server.UsernameIsEmail, tc.want)
			}
			// The [auth] mirror must agree so either key can be inspected.
			if cfg.Auth.AllowFullEmailUsername == nil {
				t.Fatal("Auth.AllowFullEmailUsername should never be nil after LoadConfig")
			}
			if *cfg.Auth.AllowFullEmailUsername != tc.want {
				t.Errorf("Auth.AllowFullEmailUsername = %v; want %v", *cfg.Auth.AllowFullEmailUsername, tc.want)
			}
		})
	}
}

// TestLoadConfig_EncryptionKeyValidation asserts LoadConfig fails fast on a
// wrong-length [encryption] key, accepts valid AES lengths, and tolerates an
// empty key (warning only) so the minimal standalone config still loads.
func TestLoadConfig_EncryptionKeyValidation(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty key warns not fatal", "", false},
		{"16-byte ok", "0123456789abcdef", false},
		{"24-byte ok", "0123456789abcdef01234567", false},
		{"32-byte ok", "0123456789abcdef0123456789abcdef", false},
		{"wrong length fatal", "too-short", true},
		{"31-byte fatal", "0123456789abcdef0123456789abcde", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := minimalIMAP
			if tc.key != "" {
				body += "\n[encryption]\nkey = \"" + tc.key + "\"\n"
			}
			_, err := LoadConfig(writeTempConfig(t, body))
			if tc.wantErr && err == nil {
				t.Fatalf("LoadConfig with key %q = nil; want error", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("LoadConfig with key %q = %v; want nil", tc.key, err)
			}
		})
	}
}

// makeConfig builds a Config struct directly (no TOML file) so tests are
// fast and hermetic.
func makeConfig(frameAncestors string, sslEnabled bool, sslDomain string) *Config {
	c := &Config{}
	c.Server.FrameAncestors = frameAncestors
	c.SSL.Enabled = sslEnabled
	c.SSL.Domain = sslDomain
	c.SSL.HSTSMaxAge = 31536000
	return c
}

func TestGetSecurityHeaders_FrameAncestorsSet(t *testing.T) {
	ancestors := "'self' http://localhost:8080"
	h := makeConfig(ancestors, false, "").GetSecurityHeaders()

	csp, hasCsp := h["Content-Security-Policy"]
	if !hasCsp {
		t.Fatal("expected Content-Security-Policy header to be present")
	}
	// CSP must include both the full policy directives AND the frame-ancestors.
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"frame-ancestors " + ancestors,
	} {
		if !containsStr(csp, want) {
			t.Errorf("Content-Security-Policy = %q; missing expected directive %q", csp, want)
		}
	}

	if _, hasXFO := h["X-Frame-Options"]; hasXFO {
		t.Error("expected X-Frame-Options to be absent when FrameAncestors is set")
	}
}

func TestGetSecurityHeaders_FrameAncestorsEmpty(t *testing.T) {
	h := makeConfig("", false, "").GetSecurityHeaders()

	xfo, hasXFO := h["X-Frame-Options"]
	if !hasXFO {
		t.Fatal("expected X-Frame-Options header to be present when FrameAncestors is empty")
	}
	if xfo != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q; want %q", xfo, "SAMEORIGIN")
	}

	// CSP must now always be present (with script-src protection) even when
	// FrameAncestors is empty; the frame-ancestors directive defaults to 'self'.
	csp, hasCsp := h["Content-Security-Policy"]
	if !hasCsp {
		t.Fatal("expected Content-Security-Policy to be present")
	}
	for _, want := range []string{
		"default-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'self'",
	} {
		if !containsStr(csp, want) {
			t.Errorf("Content-Security-Policy = %q; missing expected directive %q", csp, want)
		}
	}
}

// containsStr is a test helper that checks whether s contains substr.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestGetSecurityHeaders_XContentTypeOptionsAlwaysPresent(t *testing.T) {
	// With FrameAncestors set
	h1 := makeConfig("'self'", false, "").GetSecurityHeaders()
	if v, ok := h1["X-Content-Type-Options"]; !ok || v != "nosniff" {
		t.Errorf("X-Content-Type-Options with FrameAncestors: got %q ok=%v; want nosniff", v, ok)
	}

	// Without FrameAncestors
	h2 := makeConfig("", false, "").GetSecurityHeaders()
	if v, ok := h2["X-Content-Type-Options"]; !ok || v != "nosniff" {
		t.Errorf("X-Content-Type-Options without FrameAncestors: got %q ok=%v; want nosniff", v, ok)
	}
}

func TestGetSecurityHeaders_HSTSOnlyWhenSSLEnabledAndDomainSet(t *testing.T) {
	// SSL enabled + domain set → HSTS present
	h := makeConfig("", true, "example.com").GetSecurityHeaders()
	if _, ok := h["Strict-Transport-Security"]; !ok {
		t.Error("expected Strict-Transport-Security when SSL enabled and domain set")
	}

	// SSL enabled but no domain → HSTS absent
	h2 := makeConfig("", true, "").GetSecurityHeaders()
	if _, ok := h2["Strict-Transport-Security"]; ok {
		t.Error("expected no Strict-Transport-Security when SSL enabled but domain is empty")
	}

	// SSL disabled + domain set → HSTS absent
	h3 := makeConfig("", false, "example.com").GetSecurityHeaders()
	if _, ok := h3["Strict-Transport-Security"]; ok {
		t.Error("expected no Strict-Transport-Security when SSL disabled")
	}
}

// TestLoadConfig_CacheFolderDefault asserts LoadConfig defaults Cache.Folder to
// "./cache" when a config omits [cache] — so outbound attachment staging works
// out of the box (the wave-38 "attachments seem broken" root cause). An explicit
// [cache] folder must still override the default.
func TestLoadConfig_CacheFolderDefault(t *testing.T) {
	// No [cache] block → default applies.
	cfg, err := LoadConfig(writeTempConfig(t, minimalIMAP))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Cache.Folder != "./cache" {
		t.Fatalf("Cache.Folder default = %q, want \"./cache\" (staging would 503 without it)", cfg.Cache.Folder)
	}

	// Explicit [cache] folder overrides the default.
	cfg2, err := LoadConfig(writeTempConfig(t, minimalIMAP+"\n[cache]\nfolder = \"/var/lib/lilmail/cache\"\n"))
	if err != nil {
		t.Fatalf("LoadConfig (override): %v", err)
	}
	if cfg2.Cache.Folder != "/var/lib/lilmail/cache" {
		t.Fatalf("Cache.Folder override = %q, want the configured value", cfg2.Cache.Folder)
	}
}

// TestLoadConfig_IMAPTLSDefaultAndOverride verifies the IMAP `tls` field is
// parsed: it defaults to true (implicit-TLS / imaps), and an explicit
// `tls = false` selects plain IMAP. Regression for #8 — the field was shown in
// config.toml.example but was absent from IMAPConfig, so it was silently ignored
// and every connection used TLS, making plain IMAP fail with
// "tls: first record does not look like a TLS handshake".
func TestLoadConfig_IMAPTLSDefaultAndOverride(t *testing.T) {
	// No `tls` key → secure default (true).
	cfg, err := LoadConfig(writeTempConfig(t, minimalIMAP))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.IMAP.TLS {
		t.Fatalf("IMAP.TLS default = false, want true (implicit-TLS default)")
	}

	// Explicit `tls = false` → plain IMAP (the #8 fix: this must now be honored).
	cfgPlain, err := LoadConfig(writeTempConfig(t, minimalIMAP+"tls = false\n"))
	if err != nil {
		t.Fatalf("LoadConfig (tls=false): %v", err)
	}
	if cfgPlain.IMAP.TLS {
		t.Fatalf("IMAP.TLS with `tls = false` = true, want false (#8: plain IMAP must be honored)")
	}

	// Explicit `tls = true` → TLS.
	cfgTLS, err := LoadConfig(writeTempConfig(t, minimalIMAP+"tls = true\n"))
	if err != nil {
		t.Fatalf("LoadConfig (tls=true): %v", err)
	}
	if !cfgTLS.IMAP.TLS {
		t.Fatalf("IMAP.TLS with `tls = true` = false, want true")
	}
}
