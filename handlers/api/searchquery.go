// handlers/api/searchquery.go
//
// Server-side Gmail-style search-operator parser.
//
// The web client sends Gmail-flavoured operator queries (e.g.
// `from:alice subject:"quarterly report" has:attachment is:unread -draft`).
// lilmail delegates the search to the account's own IMAP server via IMAP SEARCH.
// This file maps the operator grammar onto an *imap.SearchCriteria program so the
// IMAP server does the filtering natively, instead of a single raw TEXT match.
//
// Operator -> IMAP criterion mapping (see parseSearchQuery):
//
//	from:X              -> HEADER FROM X        (Header["FROM"])
//	to:X                -> HEADER TO X          (Header["TO"])
//	cc:X                -> HEADER CC X          (Header["CC"])
//	bcc:X               -> HEADER BCC X         (Header["BCC"])
//	subject:X           -> SUBJECT X            (Header["SUBJECT"])
//	has:attachment      -> HEADER Content-Type multipart   (best-effort, see below)
//	is:unread           -> UNSEEN               (WithoutFlags \Seen)
//	is:read             -> SEEN                 (WithFlags \Seen)
//	is:starred          -> FLAGGED              (WithFlags \Flagged)
//	is:unstarred        -> UNFLAGGED            (WithoutFlags \Flagged)
//	in:<folder>         -> switches the searched mailbox (not a criterion)
//	before:<date>       -> BEFORE <date>        (internal-date, Before)
//	after:<date>        -> SINCE <date>         (internal-date, Since)
//	older_than:<n><unit>-> BEFORE now-n         (Before)
//	newer_than:<n><unit>-> SINCE now-n          (Since)
//	"quoted phrase"     -> TEXT "quoted phrase" (header+body)
//	free text           -> TEXT word            (header+body)
//	-term               -> NOT ( term )         (negation of any of the above)
//
// Multiple positive terms are ANDed (Gmail's default), which is the natural
// semantics of a single imap.SearchCriteria: every populated field must match.
//
// INJECTION SAFETY. Every operator/free-text VALUE is placed into a
// string-typed field of imap.SearchCriteria (Header values, Body, Text). When
// go-imap serialises the SEARCH command it runs those through
// writeQuotedOrLiteral: ASCII values are emitted with strconv.Quote (which
// escapes embedded " and \), and any value containing a control byte (CR, LF,
// NUL, ...) is emitted as a length-prefixed IMAP literal ({N}<CRLF>bytes) whose
// body the server consumes as opaque data. In neither case can a value break
// out of its atom to inject an IMAP command. Flag atoms (\Seen, \Flagged) are
// NEVER derived from user input — only from the fixed constants below — so the
// RawString KEYWORD path can't be poisoned. As defence-in-depth we additionally
// strip CR/LF/NUL and bound the length of every value in sanitizeValue.
//
// BEST-EFFORT. has:attachment has no dedicated IMAP SEARCH key. We approximate
// it with `HEADER Content-Type multipart`, which matches messages whose
// top-level Content-Type is multipart/* (the usual shape of a message carrying
// an attachment). This over-matches (multipart/alternative with no real
// attachment) and under-matches (single-part messages with an inline file are
// rare but possible); it is the standard heuristic and is documented as
// best-effort. Servers that don't index the Content-Type header will simply
// return nothing extra for this clause.
package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

const (
	// maxSearchQueryLen bounds the raw query we will parse. Anything longer is
	// truncated before tokenising, so a pathological query can't blow up the
	// tokeniser or the generated SEARCH command.
	maxSearchQueryLen = 1024
	// maxSearchValueLen bounds a single operator/free-text value.
	maxSearchValueLen = 256
)

// knownOperators is the set of recognised operator prefixes. A `word:value`
// token whose word is NOT in this set degrades to free text (the whole
// `word:value` is searched as text) — unknown operators never error.
var knownOperators = map[string]bool{
	"from":       true,
	"to":         true,
	"cc":         true,
	"bcc":        true,
	"subject":    true,
	"has":        true,
	"is":         true,
	"in":         true,
	"before":     true,
	"after":      true,
	"older_than": true,
	"newer_than": true,
}

// searchToken is one lexical unit of a query.
type searchToken struct {
	neg    bool   // preceded by '-'
	key    string // operator name, lower-cased; "" for a free-text term
	value  string // operator value or free-text word/phrase
	phrase bool   // value came from a "quoted string"
}

// tokenizeSearch splits a query into tokens, honouring "quoted phrases",
// operator:value pairs (where the value may itself be quoted, e.g.
// from:"john doe"), and a leading '-' negation. It is deliberately tolerant:
// unbalanced quotes read to end-of-string, and stray characters become
// free-text tokens.
func tokenizeSearch(q string) []searchToken {
	var toks []searchToken
	r := []rune(q)
	i, n := 0, len(r)
	for i < n {
		// Skip whitespace.
		if r[i] == ' ' || r[i] == '\t' {
			i++
			continue
		}
		neg := false
		if r[i] == '-' {
			// A '-' only negates when it prefixes a term (not standalone).
			if i+1 < n && r[i+1] != ' ' && r[i+1] != '\t' {
				neg = true
				i++
			} else {
				i++
				continue
			}
		}
		// Leading quote: a bare quoted phrase.
		if r[i] == '"' {
			val, ni := readQuoted(r, i)
			toks = append(toks, searchToken{neg: neg, value: val, phrase: true})
			i = ni
			continue
		}
		// Read a bareword up to whitespace or ':'.
		start := i
		for i < n && r[i] != ' ' && r[i] != '\t' && r[i] != ':' {
			i++
		}
		word := string(r[start:i])
		if i < n && r[i] == ':' {
			// operator:value
			key := strings.ToLower(word)
			i++ // consume ':'
			var val string
			var phrase bool
			if i < n && r[i] == '"' {
				val, i = readQuoted(r, i)
				phrase = true
			} else {
				vs := i
				for i < n && r[i] != ' ' && r[i] != '\t' {
					i++
				}
				val = string(r[vs:i])
			}
			if knownOperators[key] {
				toks = append(toks, searchToken{neg: neg, key: key, value: val, phrase: phrase})
			} else {
				// Unknown operator -> degrade to free text ("word:value").
				txt := word + ":" + val
				toks = append(toks, searchToken{neg: neg, value: txt})
			}
			continue
		}
		// Plain free-text word.
		toks = append(toks, searchToken{neg: neg, value: word})
	}
	return toks
}

// readQuoted reads a "double-quoted" run starting at r[i] (which must be '"')
// and returns the unquoted contents and the index just past the closing quote
// (or end-of-input if the quote is unbalanced).
func readQuoted(r []rune, i int) (string, int) {
	i++ // consume opening quote
	start := i
	for i < len(r) && r[i] != '"' {
		i++
	}
	val := string(r[start:i])
	if i < len(r) {
		i++ // consume closing quote
	}
	return val, i
}

// sanitizeValue strips characters that could affect IMAP framing (defence in
// depth on top of go-imap's own quoting/literal encoding) and bounds length.
func sanitizeValue(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1 // drop
		}
		return r
	}, s)
	if len(s) > maxSearchValueLen {
		s = s[:maxSearchValueLen]
	}
	return s
}

// parseSearchQuery turns a Gmail-style operator query into an IMAP search
// program. It returns the criteria to run and, if the query contained an
// `in:<folder>` operator, the mailbox to search instead of the caller's
// default (empty string means "no override").
//
// When the query contains no operators, no quoted phrase and no negation, the
// result is the historical raw-text fallback: a single TEXT match over the
// whole (trimmed) query — preserving prior behaviour byte-for-byte for simple
// queries.
func parseSearchQuery(query string) (*imap.SearchCriteria, string) {
	if len(query) > maxSearchQueryLen {
		query = query[:maxSearchQueryLen]
	}
	toks := tokenizeSearch(query)

	// Detect the "pure raw text" case: every token is a positive free-text
	// bareword (no operators, phrases or negation). Preserve the legacy
	// single-TEXT-match behaviour for those.
	structured := false
	for _, t := range toks {
		if t.key != "" || t.phrase || t.neg {
			structured = true
			break
		}
	}
	if !structured {
		crit := imap.NewSearchCriteria()
		if q := sanitizeValue(strings.TrimSpace(query)); q != "" {
			crit.Text = []string{q}
		}
		return crit, ""
	}

	crit := imap.NewSearchCriteria()
	folderOverride := ""

	for _, t := range toks {
		val := sanitizeValue(t.value)

		// Build the criterion this token contributes, into `dst`. For a
		// negated token we build into a fresh sub-criteria and attach it under
		// NOT; for a positive token we build directly into `crit`.
		dst := crit
		var neg *imap.SearchCriteria
		if t.neg {
			neg = imap.NewSearchCriteria()
			dst = neg
		}

		applied := true
		switch t.key {
		case "from":
			dst.Header.Add("FROM", val)
		case "to":
			dst.Header.Add("TO", val)
		case "cc":
			dst.Header.Add("CC", val)
		case "bcc":
			dst.Header.Add("BCC", val)
		case "subject":
			dst.Header.Add("SUBJECT", val)
		case "has":
			switch strings.ToLower(val) {
			case "attachment", "attachments", "attach", "file":
				// Best-effort: match multipart top-level Content-Type.
				dst.Header.Add("Content-Type", "multipart")
			default:
				applied = false // unknown has:<x> -> free text
			}
		case "is":
			switch strings.ToLower(val) {
			case "unread", "unseen":
				dst.WithoutFlags = append(dst.WithoutFlags, imap.SeenFlag)
			case "read", "seen":
				dst.WithFlags = append(dst.WithFlags, imap.SeenFlag)
			case "starred", "flagged":
				dst.WithFlags = append(dst.WithFlags, imap.FlaggedFlag)
			case "unstarred", "unflagged":
				dst.WithoutFlags = append(dst.WithoutFlags, imap.FlaggedFlag)
			default:
				applied = false // unknown is:<x> -> free text
			}
		case "in":
			// Mailbox switch, not a criterion. Not negatable.
			if f := resolveSearchFolder(val); f != "" && !t.neg {
				folderOverride = f
			}
			continue
		case "before":
			if d, ok := parseSearchDate(val); ok {
				dst.Before = d
			} else {
				applied = false
			}
		case "after":
			if d, ok := parseSearchDate(val); ok {
				dst.Since = d
			} else {
				applied = false
			}
		case "older_than":
			if d, ok := parseRelativeDate(val); ok {
				dst.Before = d
			} else {
				applied = false
			}
		case "newer_than":
			if d, ok := parseRelativeDate(val); ok {
				dst.Since = d
			} else {
				applied = false
			}
		default:
			// Free-text term (t.key == "").
			applied = false
		}

		if !applied {
			// Couldn't map as an operator (unknown value / unparseable date /
			// plain word): fall back to a TEXT match so nothing is silently
			// dropped. Rebuild the display value for degraded operators.
			text := val
			if t.key != "" {
				text = sanitizeValue(t.key + ":" + t.value)
			}
			if text == "" {
				continue
			}
			if t.neg {
				// neg is a fresh criteria; put TEXT there.
				neg = imap.NewSearchCriteria()
				neg.Text = []string{text}
				crit.Not = append(crit.Not, neg)
			} else {
				crit.Text = append(crit.Text, text)
			}
			continue
		}

		if t.neg {
			crit.Not = append(crit.Not, neg)
		}
	}

	return crit, folderOverride
}

// resolveSearchFolder maps an in:<folder> value to a mailbox name. Common
// aliases are normalised; everything else is passed through as-is (sanitised).
// The value is only ever used as a mailbox name in Select — never spliced into
// a SEARCH atom — but we still sanitise to keep the SELECT argument clean.
func resolveSearchFolder(v string) string {
	v = sanitizeValue(strings.TrimSpace(v))
	switch strings.ToLower(v) {
	case "":
		return ""
	case "inbox":
		return "INBOX"
	default:
		return v
	}
}

// parseSearchDate parses a Gmail-style date (YYYY/MM/DD or YYYY-MM-DD).
func parseSearchDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006/01/02", "2006-01-02", "2006/1/2", "2006-1-2"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseRelativeDate parses newer_than/older_than values like "7d", "2m", "1y"
// (days, months, years) and returns the cutoff instant (now minus the span).
func parseRelativeDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if len(v) < 2 {
		return time.Time{}, false
	}
	unit := v[len(v)-1]
	num, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || num < 0 {
		return time.Time{}, false
	}
	now := time.Now()
	switch unit {
	case 'd', 'D':
		return now.AddDate(0, 0, -num), true
	case 'm', 'M':
		return now.AddDate(0, -num, 0), true
	case 'y', 'Y':
		return now.AddDate(-num, 0, 0), true
	default:
		return time.Time{}, false
	}
}
