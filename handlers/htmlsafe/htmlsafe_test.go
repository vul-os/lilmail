package htmlsafe

import (
	"strings"
	"testing"
)

// TestSanitizeSnippet_NeutralisesActiveContent feeds representative stored-XSS
// payloads through the sanitizer used for the Edit-Draft compose slot (and for
// outgoing signatures) and asserts that no active construct survives.
func TestSanitizeSnippet_NeutralisesActiveContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"img onerror", `<img src=x onerror=alert(1)>`},
		{"img onerror quoted", `<img src="x" onerror="alert(document.cookie)">`},
		{"script element", `<script>alert(1)</script>`},
		{"svg onload", `<svg onload=alert(1)></svg>`},
		{"anchor javascript scheme", `<a href="javascript:alert(1)">click</a>`},
		{"iframe", `<iframe src="https://evil.example"></iframe>`},
		{"style element", `<style>body{background:url(https://evil.example)}</style>`},
		{"onclick on benign tag", `<b onclick="steal()">bold</b>`},
		{"slash-separated handler", `<a/onmouseover=alert(1)>x</a>`},
		{"entity-obfuscated js scheme", `<a href="&#106;avascript:alert(1)">x</a>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := strings.ToLower(SanitizeSnippet(tc.in))
			if strings.Contains(out, "onerror") ||
				strings.Contains(out, "onload") ||
				strings.Contains(out, "onclick") ||
				strings.Contains(out, "onmouseover") {
				t.Errorf("event handler survived: %q -> %q", tc.in, out)
			}
			if strings.Contains(out, "<script") {
				t.Errorf("<script> survived: %q -> %q", tc.in, out)
			}
			if strings.Contains(out, "<iframe") {
				t.Errorf("<iframe> survived: %q -> %q", tc.in, out)
			}
			if strings.Contains(out, "<style") {
				t.Errorf("<style> survived: %q -> %q", tc.in, out)
			}
			if strings.Contains(out, "<svg") {
				t.Errorf("<svg> survived: %q -> %q", tc.in, out)
			}
			if strings.Contains(out, "javascript:") {
				t.Errorf("javascript: URL survived: %q -> %q", tc.in, out)
			}
		})
	}
}

// TestSanitizeSnippet_PreservesBenignFormatting verifies the sanitizer keeps
// ordinary compose formatting so editing a draft does not lose the user's markup.
func TestSanitizeSnippet_PreservesBenignFormatting(t *testing.T) {
	in := `<p>Hello <b>world</b> — see <a href="https://example.com/path">link</a><ul><li>one</li></ul></p>`
	out := SanitizeSnippet(in)
	for _, want := range []string{"<b>", "</b>", "<p>", `href="https://example.com/path"`, "<li>"} {
		if !strings.Contains(out, want) {
			t.Errorf("benign formatting %q was stripped: %q -> %q", want, in, out)
		}
	}
}

// TestSanitizeSnippet_Empty verifies the empty-input fast path.
func TestSanitizeSnippet_Empty(t *testing.T) {
	if got := SanitizeSnippet(""); got != "" {
		t.Errorf("empty input should yield empty output, got %q", got)
	}
}
