package api

import "testing"

func TestParseAuthResultsFullVerdict(t *testing.T) {
	hdr := []string{
		"mx.example.com; spf=pass smtp.mailfrom=alice@sender.com; " +
			"dkim=pass header.d=sender.com; dmarc=pass (p=REJECT) header.from=sender.com",
	}
	res := ParseAuthResults(hdr)
	if res == nil {
		t.Fatal("expected a verdict")
	}
	if res.SPF != "pass" || res.DKIM != "pass" || res.DMARC != "pass" {
		t.Fatalf("verdicts wrong: %+v", res)
	}
	if res.DKIMDomain != "sender.com" {
		t.Fatalf("dkim domain: %q", res.DKIMDomain)
	}
	if res.Raw == "" {
		t.Fatal("raw header should be preserved")
	}
}

func TestParseAuthResultsFail(t *testing.T) {
	res := ParseAuthResults([]string{"mx.example.com; spf=fail; dkim=none; dmarc=fail"})
	if res == nil || res.SPF != "fail" || res.DKIM != "none" || res.DMARC != "fail" {
		t.Fatalf("bad parse: %+v", res)
	}
}

func TestParseAuthResultsAbsent(t *testing.T) {
	if res := ParseAuthResults(nil); res != nil {
		t.Fatalf("nil headers should yield nil, got %+v", res)
	}
	if res := ParseAuthResults([]string{"", "  "}); res != nil {
		t.Fatalf("empty headers should yield nil, got %+v", res)
	}
	if res := ParseAuthResults([]string{"mx.example.com; none"}); res != nil {
		t.Fatalf("no recognised method should yield nil, got %+v", res)
	}
}

func TestParseAuthResultsMultipleHeadersFirstWins(t *testing.T) {
	// Two hops; the first with a recognised method wins.
	res := ParseAuthResults([]string{
		"internal.relay; (no methods here)",
		"mx.example.com; spf=softfail; dkim=pass header.d=news.example.com",
	})
	if res == nil || res.SPF != "softfail" || res.DKIM != "pass" || res.DKIMDomain != "news.example.com" {
		t.Fatalf("multi-header parse: %+v", res)
	}
}
