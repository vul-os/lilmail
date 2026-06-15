package config

import "testing"

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
