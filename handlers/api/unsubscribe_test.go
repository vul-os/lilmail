package api

import "testing"

func TestParseUnsubscribe_HTTPAndMailto(t *testing.T) {
	u := ParseUnsubscribe(
		[]string{"<https://example.com/u/abc>, <mailto:unsub@example.com?subject=unsub>"},
		nil,
	)
	if u == nil {
		t.Fatal("expected an Unsubscribe, got nil")
	}
	if u.HTTPURL != "https://example.com/u/abc" {
		t.Errorf("HTTPURL = %q", u.HTTPURL)
	}
	if u.MailtoURL != "mailto:unsub@example.com?subject=unsub" {
		t.Errorf("MailtoURL = %q", u.MailtoURL)
	}
	if u.OneClick {
		t.Error("OneClick should be false without List-Unsubscribe-Post")
	}
}

func TestParseUnsubscribe_OneClick(t *testing.T) {
	u := ParseUnsubscribe(
		[]string{"<https://example.com/u/abc>"},
		[]string{"List-Unsubscribe=One-Click"},
	)
	if u == nil || !u.OneClick {
		t.Fatalf("expected OneClick, got %+v", u)
	}
	if u.HTTPURL != "https://example.com/u/abc" {
		t.Errorf("HTTPURL = %q", u.HTTPURL)
	}
}

func TestParseUnsubscribe_OneClickRequiresHTTPS(t *testing.T) {
	// RFC 8058 one-click over plaintext http would leak the token — refuse it.
	u := ParseUnsubscribe(
		[]string{"<http://example.com/u/abc>"},
		[]string{"List-Unsubscribe=One-Click"},
	)
	if u == nil {
		t.Fatal("expected an Unsubscribe (http target still surfaced)")
	}
	if u.OneClick {
		t.Error("OneClick must be false for a plaintext http target")
	}
	if u.HTTPURL != "http://example.com/u/abc" {
		t.Errorf("HTTPURL = %q", u.HTTPURL)
	}
}

func TestParseUnsubscribe_DropsHostileSchemes(t *testing.T) {
	u := ParseUnsubscribe(
		[]string{"<javascript:alert(1)>, <data:text/html,x>, <ftp://x/y>, <mailto:a@b.com>"},
		nil,
	)
	if u == nil {
		t.Fatal("expected the mailto target to survive")
	}
	if u.HTTPURL != "" {
		t.Errorf("no http target expected, got %q", u.HTTPURL)
	}
	if u.MailtoURL != "mailto:a@b.com" {
		t.Errorf("MailtoURL = %q", u.MailtoURL)
	}
	if u.OneClick {
		t.Error("OneClick should be false")
	}
}

func TestParseUnsubscribe_OnlyHostileReturnsNil(t *testing.T) {
	if u := ParseUnsubscribe([]string{"<javascript:alert(1)>"}, nil); u != nil {
		t.Fatalf("expected nil for hostile-only header, got %+v", u)
	}
}

func TestParseUnsubscribe_Empty(t *testing.T) {
	if u := ParseUnsubscribe(nil, nil); u != nil {
		t.Fatalf("expected nil, got %+v", u)
	}
	if u := ParseUnsubscribe([]string{""}, nil); u != nil {
		t.Fatalf("expected nil for empty header, got %+v", u)
	}
}

func TestParseUnsubscribe_MailtoWithComma(t *testing.T) {
	// A mailto: query can contain a comma; bracket-scanning must not split on it.
	u := ParseUnsubscribe(
		[]string{"<mailto:unsub@example.com?subject=a,b&body=c,d>"},
		nil,
	)
	if u == nil || u.MailtoURL != "mailto:unsub@example.com?subject=a,b&body=c,d" {
		t.Fatalf("mailto with comma mis-parsed: %+v", u)
	}
}

func TestParseUnsubscribe_FoldedMultipleHeaders(t *testing.T) {
	u := ParseUnsubscribe(
		[]string{"<mailto:a@b.com>", "<https://x.example/u>"},
		[]string{"List-Unsubscribe=One-Click"},
	)
	if u == nil {
		t.Fatal("expected an Unsubscribe")
	}
	if u.HTTPURL != "https://x.example/u" || u.MailtoURL != "mailto:a@b.com" {
		t.Errorf("got %+v", u)
	}
	if !u.OneClick {
		t.Error("expected OneClick across folded headers")
	}
}
