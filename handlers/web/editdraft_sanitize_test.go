// handlers/web/editdraft_sanitize_test.go — regression guard for the Edit-Draft
// stored-XSS fix. The value returned by editableDraftHTML is what HandleEmailView
// places into the email-viewer template's data-html attribute, which
// restoreDraft() assigns via innerHTML into the compose contenteditable in the
// MAIN (un-sandboxed) app document. It MUST be defanged.
package web

import (
	"strings"
	"testing"
)

func TestEditableDraftHTML_StripsActiveContent(t *testing.T) {
	// A malicious HTML mail parked in a "Drafts"-named folder.
	raw := `<div>Draft body</div>` +
		`<img src=x onerror="fetch('/api/send',{method:'POST'})">` +
		`<script>alert(document.cookie)</script>` +
		`<svg onload=alert(1)></svg>` +
		`<a href="javascript:alert(1)">x</a>` +
		`<iframe src="https://evil.example"></iframe>` +
		`<style>body{display:none}</style>` +
		`<b onclick="steal()">bold</b>`

	out := strings.ToLower(editableDraftHTML(raw))

	for _, banned := range []string{"onerror", "onload", "onclick", "<script", "<iframe", "<style", "<svg", "javascript:"} {
		if strings.Contains(out, banned) {
			t.Errorf("edit-draft slot still contains %q: %q", banned, out)
		}
	}
	// Benign text/formatting should be preserved so the draft is still editable.
	if !strings.Contains(out, "draft body") {
		t.Errorf("benign draft content was lost: %q", out)
	}
	if !strings.Contains(out, "<b") {
		t.Errorf("benign <b> formatting was stripped: %q", out)
	}
}

func TestEditableDraftHTML_Empty(t *testing.T) {
	if got := editableDraftHTML(""); got != "" {
		t.Errorf("empty input should yield empty output, got %q", got)
	}
}
