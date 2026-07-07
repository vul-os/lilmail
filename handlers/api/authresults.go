// handlers/api/authresults.go — parse the Authentication-Results header (RFC 8601)
// into the distilled SPF/DKIM/DMARC verdict surfaced on models.Email.Auth.
//
// The header is stamped by the RECEIVING server (the boundary MTA that
// authenticated the message on delivery); lilmail does not re-verify, it merely
// surfaces the trusted receiver's verdict for a "verified sender" / "why in spam"
// badge. Parsing is intentionally lenient: a mailbox may carry several
// Authentication-Results headers (one per hop); we take the first that yields any
// recognised method and read the SPF/DKIM/DMARC result tokens out of it.
//
// A header looks like:
//
//	Authentication-Results: mx.example.com;
//	  spf=pass smtp.mailfrom=alice@sender.com;
//	  dkim=pass header.d=sender.com;
//	  dmarc=pass (p=REJECT) header.from=sender.com
//
// This is cheap and read-only — no network, no allocation beyond the small result.
package api

import (
	"regexp"
	"strings"

	"lilmail/models"
)

// authMethodRe matches "method=result", e.g. spf=pass / dkim=fail / dmarc=none.
// The result token is a bare word (letters); trailing detail (smtp.mailfrom=…,
// (p=REJECT), header.d=…) is captured separately below.
var authMethodRe = regexp.MustCompile(`(?i)\b(spf|dkim|dmarc)\s*=\s*([a-z]+)`)

// dkimDomainRe extracts the header.d= domain from a DKIM clause when present.
var dkimDomainRe = regexp.MustCompile(`(?i)header\.d\s*=\s*([A-Za-z0-9.\-]+)`)

// ParseAuthResults distills one or more Authentication-Results header values into
// an *models.AuthResults. Returns nil when no header carries a recognised method,
// so an unauthenticated/legacy message simply has Email.Auth == nil (no badge).
// headers is every Authentication-Results value on the message (a message may have
// more than one); the FIRST value yielding any recognised method wins.
func ParseAuthResults(headers []string) *models.AuthResults {
	for _, h := range headers {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		matches := authMethodRe.FindAllStringSubmatch(h, -1)
		if len(matches) == 0 {
			continue
		}
		res := &models.AuthResults{Raw: h}
		for _, m := range matches {
			method := strings.ToLower(m[1])
			result := strings.ToLower(m[2])
			switch method {
			case "spf":
				if res.SPF == "" {
					res.SPF = result
				}
			case "dkim":
				if res.DKIM == "" {
					res.DKIM = result
				}
			case "dmarc":
				if res.DMARC == "" {
					res.DMARC = result
				}
			}
		}
		if d := dkimDomainRe.FindStringSubmatch(h); d != nil {
			res.DKIMDomain = strings.ToLower(d[1])
		}
		// Only return if we actually recognised at least one verdict.
		if res.SPF != "" || res.DKIM != "" || res.DMARC != "" {
			return res
		}
	}
	return nil
}
