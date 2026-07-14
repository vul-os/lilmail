package jsonapi

import (
	"context"
	"testing"

	"lilmail/handlers/api"
	"lilmail/models"
)

func TestDomainOfAddress(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":           "example.com",
		"Alice <alice@Example.COM>":   "example.com",
		"\"A. B\" <a@sub.example.io>": "sub.example.io",
		"nobody":                      "",
		"trailing@":                   "",
		"":                            "",
	}
	for in, want := range cases {
		if got := domainOfAddress(in); got != want {
			t.Errorf("domainOfAddress(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestAttachBrandIndicatorGate(t *testing.T) {
	// Stub the resolver seam so no DNS/network is touched; record whether it ran
	// and with what.
	var gotDomain string
	var gotDMARC bool
	ran := false
	orig := bimiResolve
	bimiResolve = func(_ *Handler, _ context.Context, domain string, dmarcPass bool) (*models.BrandIndicator, bool) {
		ran = true
		gotDomain, gotDMARC = domain, dmarcPass
		return &models.BrandIndicator{Domain: domain, Logo: "data:image/svg+xml;base64,PHN2Zy8+"}, true
	}
	defer func() { bimiResolve = orig }()

	// attach must be a no-op when h.bimi is nil (defensive guard) — verify first.
	h := &Handler{}
	email := &models.Email{From: "brand@example.com", Auth: &models.AuthResults{DMARC: "pass"}}
	h.attachBrandIndicator(context.Background(), email)
	if email.Brand != nil || ran {
		t.Fatal("with a nil resolver, attach must be a no-op")
	}

	// Give the handler a (non-nil) resolver; the seam is stubbed so it won't dial.
	h.bimi = api.NewBIMIResolver()
	ran = false

	// No Auth → fail closed (no lookup).
	e1 := &models.Email{From: "brand@example.com"}
	h.attachBrandIndicator(context.Background(), e1)
	if e1.Brand != nil || ran {
		t.Fatal("no Auth → no brand, no lookup")
	}

	// DMARC != pass → fail closed.
	e2 := &models.Email{From: "brand@example.com", Auth: &models.AuthResults{DMARC: "fail"}}
	h.attachBrandIndicator(context.Background(), e2)
	if e2.Brand != nil || ran {
		t.Fatal("DMARC fail → no brand, no lookup")
	}

	// DMARC pass → lookup runs with the From domain + dmarcPass=true, brand attached.
	e3 := &models.Email{From: "Brand <brand@Example.com>", Auth: &models.AuthResults{DMARC: "PASS"}}
	h.attachBrandIndicator(context.Background(), e3)
	if !ran {
		t.Fatal("DMARC pass → resolver must run")
	}
	if gotDomain != "example.com" || !gotDMARC {
		t.Fatalf("resolver called with domain=%q dmarc=%v", gotDomain, gotDMARC)
	}
	if e3.Brand == nil || e3.Brand.Domain != "example.com" {
		t.Fatalf("brand not attached: %+v", e3.Brand)
	}
}
