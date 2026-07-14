package jsonapi

// bimi.go — attach a VERIFIED sender brand logo (BIMI) to a single-message read.
//
// This is the /v1 seam over handlers/api's BIMIResolver. It is FAIL-CLOSED: a
// logo is attached ONLY when the message carries a DMARC=pass verdict (which
// implies SPF-or-DKIM alignment with the From domain). An unauthenticated sender
// never gets a logo, so the logo can only ever be a positive trust signal — never
// a phishing aid. The resolver additionally screens the outbound fetch (SSRF) and
// sanitizes the SVG, and the client only renders the result in an <img> (image
// context, no script execution). See handlers/api/bimi.go.

import (
	"context"
	"strings"

	"lilmail/models"
)

// bimiResolve is the resolver seam (package var so tests can stub it without DNS
// or network). It returns the verified indicator, or (nil,false). The default
// serves from cache and warms in the background on a miss, so a message open
// never blocks on DNS/HTTP — the logo appears from cache on a subsequent open.
var bimiResolve = func(h *Handler, _ context.Context, fromDomain string, dmarcPass bool) (*models.BrandIndicator, bool) {
	return h.bimi.ResolveCachedOrWarm(fromDomain, dmarcPass)
}

// attachBrandIndicator stamps email.Brand with the sender's verified BIMI logo
// when — and only when — the message passed DMARC. Best-effort and additive: no
// From domain, no DMARC pass, no BIMI record, or any fetch/sanitize failure all
// leave email.Brand nil (no indicator), which is the safe default.
func (h *Handler) attachBrandIndicator(ctx context.Context, email *models.Email) {
	if h == nil || h.bimi == nil || email == nil {
		return
	}
	// FAIL-CLOSED gate: require a DMARC pass verdict from the receiving MTA. DMARC
	// pass ⇒ the From domain authenticated and aligned, the exact property that
	// makes a domain's BIMI logo trustworthy.
	if email.Auth == nil || !strings.EqualFold(strings.TrimSpace(email.Auth.DMARC), "pass") {
		return
	}
	domain := domainOfAddress(email.From)
	if domain == "" {
		return
	}
	if ind, ok := bimiResolve(h, ctx, domain, true); ok && ind != nil {
		email.Brand = ind
	}
}

// domainOfAddress extracts the domain from an email address that may be bare
// ("a@b.com") or in display form ("Name <a@b.com>"). Returns "" when it can't
// find a plausible domain.
func domainOfAddress(from string) string {
	s := strings.TrimSpace(from)
	if s == "" {
		return ""
	}
	// Strip a display form "Name <addr>" down to addr.
	if lt := strings.LastIndexByte(s, '<'); lt >= 0 {
		if gt := strings.IndexByte(s[lt+1:], '>'); gt >= 0 {
			s = s[lt+1 : lt+1+gt]
		}
	}
	at := strings.LastIndexByte(s, '@')
	if at < 0 || at == len(s)-1 {
		return ""
	}
	dom := strings.TrimSpace(s[at+1:])
	dom = strings.Trim(dom, "> ")
	return strings.ToLower(dom)
}
