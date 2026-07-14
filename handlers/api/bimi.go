// handlers/api/bimi.go — BIMI (Brand Indicators for Message Identification):
// resolve a VERIFIED sender brand logo, fail-closed on authentication.
//
// WHY THIS IS PRIVACY-SAFE (and why we do it server-side): the logo is named in
// the SENDER's own DNS (default._bimi.<from-domain> TXT, l= tag) and is only
// trusted when THAT sender passed DMARC. It therefore leaks NOTHING about the
// recipient — the lookup is keyed on the sender's domain and their own
// authentication, not on who received the mail. We fetch and cache it once per
// domain from the server (not the recipient's browser), so a sender can never see
// a per-recipient beacon. This is the deliberate opposite of a Gravatar-style
// third-party avatar lookup (which WOULD tell an outside server who is emailing
// the user on every message) — we implement none of that.
//
// WHY IT IS AN ANTI-PHISHING SIGNAL: a brand logo appears ONLY when the message
// genuinely authenticated (DMARC pass ⇒ SPF-or-DKIM aligned with the From domain).
// A look-alike/spoofed sender cannot produce the logo, so its presence is a
// positive trust cue and its ABSENCE for a "known brand" is a warning. Because a
// wrongly-shown logo would be a phishing AID, every gate here FAILS CLOSED: no
// DMARC pass → no lookup; no/invalid BIMI record → no logo; fetch/screen/sanitize
// failure → no logo.
//
// SECURITY (this fetches a SENDER-CONTROLLED URL, so it is the SSRF surface):
//   - Only https is followed; the connect IP is screened (screenDialIP with
//     allowPrivate=false) so a BIMI l= URL can never probe loopback/private/
//     link-local space or a cloud metadata endpoint, and a DNS-rebind is rejected
//     at connect time (net.Dialer.Control), not merely at parse time.
//   - Redirects are refused (a redirect could target an internal host post-screen).
//   - The response is size-capped (maxBIMIBytes) and time-bounded.
//   - The SVG is sanitized to the BIMI SVG Tiny "Portable/Secure" spirit: any
//     <script>/<foreignObject>/event handler/external reference/`javascript:` makes
//     the WHOLE logo fail closed (we show nothing rather than a risky image). On
//     top of that the client only ever renders it in an <img> (image context — no
//     script, no external fetch).
package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"lilmail/models"
)

const (
	// maxBIMIBytes caps a fetched logo (BIMI SVG-P/S logos are small; 64 KiB is
	// generous). A larger body is refused (fail-closed → no logo).
	maxBIMIBytes = 64 << 10
	// bimiFetchTimeout bounds the whole DNS+HTTP resolve.
	bimiFetchTimeout = 6 * time.Second
	// bimiTTL is how long a resolved (positive OR negative) result is cached per
	// domain. Positive keeps us from re-fetching a brand's logo on every open;
	// negative keeps a non-BIMI/unreachable domain from being probed repeatedly.
	bimiPosTTL = 12 * time.Hour
	bimiNegTTL = 1 * time.Hour
	// bimiCacheMax bounds the cache so a flood of distinct sender domains cannot
	// grow it without limit (oldest-inserted entries are dropped).
	bimiCacheMax = 4096
)

// BIMIResolver resolves and caches verified brand logos per sender domain. It is
// safe for concurrent use. A zero value is not usable — build one with NewBIMIResolver.
type BIMIResolver struct {
	txt func(ctx context.Context, name string) ([]string, error) // DNS TXT seam (tests)
	get func(ctx context.Context, url string) ([]byte, string, error)

	mu       sync.Mutex
	cache    map[string]bimiEntry
	order    []string
	inflight map[string]struct{} // domains currently being warmed (stampede guard)
}

type bimiEntry struct {
	ind    *models.BrandIndicator // nil = negative (no BIMI / failed) result
	expiry time.Time
}

// NewBIMIResolver builds a resolver using real DNS + a screened HTTP fetch.
func NewBIMIResolver() *BIMIResolver {
	return &BIMIResolver{
		txt:      defaultTXTLookup,
		get:      screenedHTTPGet,
		cache:    map[string]bimiEntry{},
		inflight: map[string]struct{}{},
	}
}

// Resolve returns the verified brand indicator for fromDomain, or (nil,false).
//
// FAIL-CLOSED GATE: it returns nothing unless dmarcPass is true. DMARC "pass"
// means SPF or DKIM authenticated AND aligned with the From domain — exactly the
// property that makes a BIMI logo trustworthy. An unauthenticated sender never
// gets a logo, so the logo can never be a phishing aid.
func (r *BIMIResolver) Resolve(ctx context.Context, fromDomain string, dmarcPass bool) (*models.BrandIndicator, bool) {
	if r == nil || !dmarcPass {
		return nil, false
	}
	domain := normalizeDomain(fromDomain)
	if domain == "" {
		return nil, false
	}
	if e, ok := r.cacheGet(domain); ok {
		return e, e != nil
	}
	ind := r.resolveUncached(ctx, domain)
	r.cachePut(domain, ind)
	return ind, ind != nil
}

// ResolveCachedOrWarm returns a verified brand indicator ONLY if it is already
// cached; on a miss it kicks off a background warm (deduplicated per domain) and
// returns (nil,false) immediately. This keeps the message-open path from ever
// blocking on DNS/HTTP: a brand's logo appears from cache on the next open. Same
// fail-closed DMARC gate as Resolve.
func (r *BIMIResolver) ResolveCachedOrWarm(fromDomain string, dmarcPass bool) (*models.BrandIndicator, bool) {
	if r == nil || !dmarcPass {
		return nil, false
	}
	domain := normalizeDomain(fromDomain)
	if domain == "" {
		return nil, false
	}
	if e, ok := r.cacheGet(domain); ok {
		return e, e != nil
	}
	// Not cached — warm in the background (once per domain at a time).
	r.mu.Lock()
	if _, busy := r.inflight[domain]; busy {
		r.mu.Unlock()
		return nil, false
	}
	r.inflight[domain] = struct{}{}
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.inflight, domain)
			r.mu.Unlock()
		}()
		ind := r.resolveUncached(context.Background(), domain)
		r.cachePut(domain, ind)
	}()
	return nil, false
}

func (r *BIMIResolver) resolveUncached(ctx context.Context, domain string) *models.BrandIndicator {
	ctx, cancel := context.WithTimeout(ctx, bimiFetchTimeout)
	defer cancel()

	logoURL, vmcURL := r.lookupBIMIRecord(ctx, domain)
	if logoURL == "" {
		return nil // no usable BIMI record → no logo (fail-closed)
	}
	// The logo location MUST be https (BIMI requires it) — never fetch anything else.
	if !strings.HasPrefix(strings.ToLower(logoURL), "https://") {
		return nil
	}
	body, ctype, err := r.get(ctx, logoURL)
	if err != nil || len(body) == 0 {
		return nil
	}
	// Content-Type should be SVG; some servers mislabel, so we also sniff the bytes.
	if !looksLikeSVG(body, ctype) {
		return nil
	}
	if !svgIsSafe(body) {
		return nil // non-compliant / risky SVG → show nothing (fail-closed)
	}
	logo := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(body)
	return &models.BrandIndicator{Domain: domain, Logo: logo, VMC: vmcURL != ""}
}

// lookupBIMIRecord fetches default._bimi.<domain> TXT and returns the l= (logo)
// and a= (VMC) URLs. Returns empty logo when there is no valid v=BIMI1 record.
func (r *BIMIResolver) lookupBIMIRecord(ctx context.Context, domain string) (logoURL, vmcURL string) {
	recs, err := r.txt(ctx, "default._bimi."+domain)
	if err != nil {
		return "", ""
	}
	for _, rec := range recs {
		l, a, ok := parseBIMIRecord(rec)
		if ok {
			return l, a
		}
	}
	return "", ""
}

// parseBIMIRecord parses a BIMI TXT record: "v=BIMI1; l=<https logo>; a=<https vmc>".
// Requires the v=BIMI1 version tag. l= may legitimately be empty (a "declined"
// record) — we treat that as no logo. Tag names are case-insensitive.
func parseBIMIRecord(rec string) (logoURL, vmcURL string, ok bool) {
	hasVersion := false
	for _, part := range strings.Split(rec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		tag := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := strings.TrimSpace(part[eq+1:])
		switch tag {
		case "v":
			if strings.EqualFold(val, "BIMI1") {
				hasVersion = true
			}
		case "l":
			logoURL = val
		case "a":
			vmcURL = val
		}
	}
	if !hasVersion {
		return "", "", false
	}
	return logoURL, vmcURL, true
}

// --- cache ---------------------------------------------------------------------

func (r *BIMIResolver) cacheGet(domain string) (*models.BrandIndicator, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[domain]
	if !ok || time.Now().After(e.expiry) {
		return nil, false
	}
	return e.ind, true
}

func (r *BIMIResolver) cachePut(domain string, ind *models.BrandIndicator) {
	ttl := bimiNegTTL
	if ind != nil {
		ttl = bimiPosTTL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.cache[domain]; !exists {
		if len(r.order) >= bimiCacheMax {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.cache, oldest)
		}
		r.order = append(r.order, domain)
	}
	r.cache[domain] = bimiEntry{ind: ind, expiry: time.Now().Add(ttl)}
}

// --- SVG safety ----------------------------------------------------------------

// looksLikeSVG reports whether body/ctype is plausibly an SVG document.
func looksLikeSVG(body []byte, ctype string) bool {
	if strings.Contains(strings.ToLower(ctype), "svg") {
		return true
	}
	head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	return strings.Contains(head, "<svg")
}

// svgForbidden are tokens that must NOT appear in a BIMI SVG-P/S logo. Any hit
// fails the whole logo closed. This is deliberately conservative: BIMI logos are
// static marks, so scripts, foreign content, external references and event
// handlers are never legitimate, and refusing them entirely is safer than trying
// to rewrite hostile markup.
var svgForbidden = []string{
	"<script", "</script", "<foreignobject", "<iframe", "<embed", "<object",
	"<use", "<image", "<audio", "<video", "<animate", "<set ", "<a ", "<a>",
	"javascript:", "data:text/html", "<!entity", "<!doctype", "<?php",
	"xlink:href=\"http", "xlink:href='http", "href=\"http", "href='http",
	"url(http", "url( http", "@import",
}

// svgIsSafe reports whether an SVG body is free of every forbidden construct,
// including any on*= event-handler attribute. Case-insensitive.
func svgIsSafe(body []byte) bool {
	s := strings.ToLower(string(body))
	// The root element must actually be an SVG.
	if !strings.Contains(s, "<svg") {
		return false
	}
	for _, bad := range svgForbidden {
		if strings.Contains(s, bad) {
			return false
		}
	}
	// Any inline event handler attribute (onload=, onclick=, onmouseover=, …).
	if containsEventHandler(s) {
		return false
	}
	return true
}

// containsEventHandler scans for an HTML/SVG event-handler attribute: whitespace
// (or a tag char) followed by `on<letters>=`. Bounded linear scan, no regexp.
func containsEventHandler(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] != 'o' || s[i+1] != 'n' {
			continue
		}
		// The char before "on" must be a separator so we don't match e.g. "button".
		if i > 0 {
			c := s[i-1]
			if !(c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' || c == '"' || c == '\'' || c == '<') {
				continue
			}
		}
		// After "on", require letters then '='.
		j := i + 2
		for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
			j++
		}
		if j > i+2 && j < len(s) {
			// Allow optional spaces before '='.
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if k < len(s) && s[k] == '=' {
				return true
			}
		}
	}
	return false
}

// --- DNS + screened HTTP -------------------------------------------------------

func defaultTXTLookup(ctx context.Context, name string) ([]string, error) {
	var res net.Resolver
	return res.LookupTXT(ctx, name)
}

// screenedHTTPGet fetches url over https with SSRF screening (screenDialIP,
// allowPrivate=false), no redirects, a size cap, and a timeout. Returns the body
// and Content-Type. It reuses the exact IP-screening the brokered-DAV/IMAP guards
// use, so a sender-controlled BIMI URL can never reach internal/metadata space.
func screenedHTTPGet(ctx context.Context, url string) ([]byte, string, error) {
	dialer := &net.Dialer{
		Timeout: bimiFetchTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				h = address
			}
			ip := net.ParseIP(h)
			if ip == nil {
				return fmt.Errorf("bimi: refusing unresolved dial address %q", address)
			}
			return screenDialIP(ip, false) // never allow private/loopback/metadata
		},
	}
	client := &http.Client{
		Timeout: bimiFetchTimeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: bimiFetchTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("bimi: refusing redirect")
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/svg+xml")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("bimi: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBIMIBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxBIMIBytes {
		return nil, "", fmt.Errorf("bimi: logo exceeds %d bytes", maxBIMIBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// normalizeDomain lowercases and trims a From domain to its registrable form as
// received. (BIMI is looked up at the exact From domain; org-domain fallback is
// out of scope.)
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	if d == "" || strings.ContainsAny(d, " \t/@") {
		return ""
	}
	return d
}
