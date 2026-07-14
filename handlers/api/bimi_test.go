package api

import (
	"context"
	"strings"
	"testing"
)

func TestParseBIMIRecord(t *testing.T) {
	l, a, ok := parseBIMIRecord("v=BIMI1; l=https://ex.com/logo.svg; a=https://ex.com/vmc.pem")
	if !ok || l != "https://ex.com/logo.svg" || a != "https://ex.com/vmc.pem" {
		t.Fatalf("valid record: ok=%v l=%q a=%q", ok, l, a)
	}
	// Case-insensitive tag names + version.
	if l, _, ok := parseBIMIRecord("V=bimi1; L=https://ex.com/x.svg"); !ok || l != "https://ex.com/x.svg" {
		t.Fatalf("case-insensitive parse failed: ok=%v l=%q", ok, l)
	}
	// Missing version → not a BIMI record.
	if _, _, ok := parseBIMIRecord("l=https://ex.com/logo.svg"); ok {
		t.Fatal("record without v=BIMI1 must be rejected")
	}
	// Declined record (empty l) parses but yields no logo.
	if l, _, ok := parseBIMIRecord("v=BIMI1; l="); !ok || l != "" {
		t.Fatalf("declined record: ok=%v l=%q", ok, l)
	}
}

func TestSVGIsSafe(t *testing.T) {
	safe := []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" fill="#0af"/></svg>`,
		`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><circle cx="10" cy="10" r="8"/></svg>`,
	}
	for _, s := range safe {
		if !svgIsSafe([]byte(s)) {
			t.Errorf("expected safe: %q", s)
		}
	}
	unsafe := []string{
		`<svg><script>alert(1)</script></svg>`,
		`<svg onload="alert(1)"><rect/></svg>`,
		`<svg><foreignObject><body xmlns="http://www.w3.org/1999/xhtml">x</body></foreignObject></svg>`,
		`<svg><image href="https://evil.example/x.png"/></svg>`,
		`<svg><use xlink:href="https://evil.example/x#a"/></svg>`,
		`<svg><a href="https://evil.example">x</a></svg>`,
		`<svg><rect fill="url(https://evil.example/x)"/></svg>`,
		`<svg><rect onclick="x()"/></svg>`,
		`<div>not an svg at all</div>`,
		`<!DOCTYPE svg [<!ENTITY x "y">]><svg/>`,
	}
	for _, s := range unsafe {
		if svgIsSafe([]byte(s)) {
			t.Errorf("expected UNSAFE (fail-closed): %q", s)
		}
	}
}

// newTestResolver builds a resolver with stubbed DNS + fetch so no network is
// touched; txtCalls/getCalls let the cache test assert on call counts.
func newTestResolver(txt func(context.Context, string) ([]string, error), get func(context.Context, string) ([]byte, string, error)) *BIMIResolver {
	return &BIMIResolver{txt: txt, get: get, cache: map[string]bimiEntry{}}
}

func TestResolveFailsClosedWithoutDMARC(t *testing.T) {
	called := false
	r := newTestResolver(
		func(context.Context, string) ([]string, error) {
			called = true
			return []string{"v=BIMI1; l=https://ex.com/l.svg"}, nil
		},
		func(context.Context, string) ([]byte, string, error) {
			called = true
			return []byte(`<svg/>`), "image/svg+xml", nil
		},
	)
	if ind, ok := r.Resolve(context.Background(), "ex.com", false); ok || ind != nil {
		t.Fatal("must return nothing when dmarcPass=false")
	}
	if called {
		t.Fatal("must NOT touch DNS/HTTP when dmarcPass=false (fail-closed before any lookup)")
	}
}

func TestResolveHappyPath(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" fill="#123"/></svg>`
	r := newTestResolver(
		func(_ context.Context, name string) ([]string, error) {
			if name != "default._bimi.brand.example" {
				t.Fatalf("unexpected TXT name %q", name)
			}
			return []string{"v=BIMI1; l=https://brand.example/logo.svg; a=https://brand.example/vmc.pem"}, nil
		},
		func(_ context.Context, url string) ([]byte, string, error) {
			if url != "https://brand.example/logo.svg" {
				t.Fatalf("unexpected fetch url %q", url)
			}
			return []byte(svg), "image/svg+xml", nil
		},
	)
	ind, ok := r.Resolve(context.Background(), "Brand.Example", true)
	if !ok || ind == nil {
		t.Fatal("expected an indicator for a valid DMARC-pass BIMI domain")
	}
	if ind.Domain != "brand.example" {
		t.Errorf("domain=%q", ind.Domain)
	}
	if !strings.HasPrefix(ind.Logo, "data:image/svg+xml;base64,") {
		t.Errorf("logo not a sanitized svg data URI: %q", ind.Logo)
	}
	if !ind.VMC {
		t.Error("VMC should be true when a= present")
	}
}

func TestResolveFailsClosedOnUnsafeSVG(t *testing.T) {
	r := newTestResolver(
		func(context.Context, string) ([]string, error) {
			return []string{"v=BIMI1; l=https://ex.com/l.svg"}, nil
		},
		func(context.Context, string) ([]byte, string, error) {
			return []byte(`<svg onload="steal()"><script>1</script></svg>`), "image/svg+xml", nil
		},
	)
	if ind, ok := r.Resolve(context.Background(), "ex.com", true); ok || ind != nil {
		t.Fatal("a script/handler-bearing SVG must yield NO logo (fail-closed)")
	}
}

func TestResolveRejectsNonHTTPSLogo(t *testing.T) {
	fetched := false
	r := newTestResolver(
		func(context.Context, string) ([]string, error) {
			return []string{"v=BIMI1; l=http://ex.com/l.svg"}, nil
		},
		func(context.Context, string) ([]byte, string, error) {
			fetched = true
			return []byte(`<svg/>`), "image/svg+xml", nil
		},
	)
	if ind, ok := r.Resolve(context.Background(), "ex.com", true); ok || ind != nil {
		t.Fatal("a plaintext http l= must be refused")
	}
	if fetched {
		t.Fatal("must not fetch a non-https logo URL")
	}
}

func TestResolveNoRecord(t *testing.T) {
	r := newTestResolver(
		func(context.Context, string) ([]string, error) { return nil, nil },
		func(context.Context, string) ([]byte, string, error) {
			t.Fatal("must not fetch without a record")
			return nil, "", nil
		},
	)
	if ind, ok := r.Resolve(context.Background(), "ex.com", true); ok || ind != nil {
		t.Fatal("no BIMI record → no logo")
	}
}

func TestResolveCachesPerDomain(t *testing.T) {
	txtCalls, getCalls := 0, 0
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`
	r := newTestResolver(
		func(context.Context, string) ([]string, error) {
			txtCalls++
			return []string{"v=BIMI1; l=https://ex.com/l.svg"}, nil
		},
		func(context.Context, string) ([]byte, string, error) {
			getCalls++
			return []byte(svg), "image/svg+xml", nil
		},
	)
	for i := 0; i < 3; i++ {
		if _, ok := r.Resolve(context.Background(), "ex.com", true); !ok {
			t.Fatalf("resolve %d failed", i)
		}
	}
	if txtCalls != 1 || getCalls != 1 {
		t.Fatalf("expected 1 DNS + 1 fetch across 3 resolves (cached), got txt=%d get=%d", txtCalls, getCalls)
	}
	// A negative result is also cached (no repeated probing of a non-BIMI domain).
	txtCalls = 0
	rn := newTestResolver(
		func(context.Context, string) ([]string, error) { txtCalls++; return nil, nil },
		func(context.Context, string) ([]byte, string, error) { return nil, "", nil },
	)
	rn.Resolve(context.Background(), "none.example", true)
	rn.Resolve(context.Background(), "none.example", true)
	if txtCalls != 1 {
		t.Fatalf("negative result should be cached: txtCalls=%d", txtCalls)
	}
}
