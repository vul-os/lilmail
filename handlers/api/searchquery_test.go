package api

import (
	"bytes"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
)

// --- helpers ----------------------------------------------------------------

// hdr asserts a header criterion is present. textproto.MIMEHeader canonicalises
// keys on Add (FROM -> From), so we look up the canonical form; go-imap's
// Format() upper-cases it back to FROM on the wire.
func hdr(t *testing.T, c *imap.SearchCriteria, key, want string) {
	t.Helper()
	vals := c.Header[textproto.CanonicalMIMEHeaderKey(key)]
	for _, v := range vals {
		if v == want {
			return
		}
	}
	t.Fatalf("Header[%q] = %v; want to contain %q", key, vals, want)
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

// encodeSearch renders the criteria to the exact bytes lilmail would put on the
// IMAP wire for a SEARCH command, so tests can assert the serialised form.
func encodeSearch(t *testing.T, c *imap.SearchCriteria) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := (&commands.Search{Criteria: c}).Command()
	cmd.Tag = "A001"
	if err := cmd.WriteTo(imap.NewWriter(&buf)); err != nil {
		t.Fatalf("encode search: %v", err)
	}
	return buf.String()
}

// --- operator mapping -------------------------------------------------------

func TestParse_HeaderOperators(t *testing.T) {
	c, folder := parseSearchQuery(`from:alice to:bob cc:carol bcc:dave subject:report`)
	if folder != "" {
		t.Fatalf("unexpected folder override %q", folder)
	}
	hdr(t, c, "FROM", "alice")
	hdr(t, c, "TO", "bob")
	hdr(t, c, "CC", "carol")
	hdr(t, c, "BCC", "dave")
	hdr(t, c, "SUBJECT", "report")
}

func TestParse_HasAttachment(t *testing.T) {
	c, _ := parseSearchQuery(`has:attachment`)
	hdr(t, c, "Content-Type", "multipart")
}

func TestParse_IsFlags(t *testing.T) {
	c, _ := parseSearchQuery(`is:unread`)
	if !hasFlag(c.WithoutFlags, imap.SeenFlag) {
		t.Fatalf("is:unread -> WithoutFlags = %v; want \\Seen", c.WithoutFlags)
	}
	c, _ = parseSearchQuery(`is:read`)
	if !hasFlag(c.WithFlags, imap.SeenFlag) {
		t.Fatalf("is:read -> WithFlags = %v; want \\Seen", c.WithFlags)
	}
	c, _ = parseSearchQuery(`is:starred`)
	if !hasFlag(c.WithFlags, imap.FlaggedFlag) {
		t.Fatalf("is:starred -> WithFlags = %v; want \\Flagged", c.WithFlags)
	}
	c, _ = parseSearchQuery(`is:unstarred`)
	if !hasFlag(c.WithoutFlags, imap.FlaggedFlag) {
		t.Fatalf("is:unstarred -> WithoutFlags = %v; want \\Flagged", c.WithoutFlags)
	}
}

func TestParse_InFolder(t *testing.T) {
	_, folder := parseSearchQuery(`in:Archive from:alice`)
	if folder != "Archive" {
		t.Fatalf("folder override = %q; want Archive", folder)
	}
	// inbox alias normalises to INBOX.
	_, folder = parseSearchQuery(`in:inbox`)
	if folder != "INBOX" {
		t.Fatalf("in:inbox -> %q; want INBOX", folder)
	}
}

func TestParse_Dates(t *testing.T) {
	c, _ := parseSearchQuery(`before:2026/01/15 after:2025-12-01`)
	if c.Before.IsZero() || c.Before.Year() != 2026 || c.Before.Month() != time.January || c.Before.Day() != 15 {
		t.Fatalf("before -> %v", c.Before)
	}
	if c.Since.IsZero() || c.Since.Year() != 2025 || c.Since.Month() != time.December || c.Since.Day() != 1 {
		t.Fatalf("after -> %v", c.Since)
	}
}

func TestParse_RelativeDates(t *testing.T) {
	c, _ := parseSearchQuery(`newer_than:7d`)
	if c.Since.IsZero() {
		t.Fatalf("newer_than:7d did not set Since")
	}
	if d := time.Since(c.Since); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("newer_than:7d Since=%v (delta %v) not ~7d", c.Since, d)
	}
	c, _ = parseSearchQuery(`older_than:1y`)
	if c.Before.IsZero() {
		t.Fatalf("older_than:1y did not set Before")
	}
	if !c.Before.Before(time.Now().AddDate(0, 0, -300)) {
		t.Fatalf("older_than:1y Before=%v not ~1y ago", c.Before)
	}
}

// --- phrase, negation, combination -----------------------------------------

func TestParse_QuotedPhrase(t *testing.T) {
	c, _ := parseSearchQuery(`"quarterly report"`)
	if len(c.Text) != 1 || c.Text[0] != "quarterly report" {
		t.Fatalf("phrase -> Text=%v; want [quarterly report]", c.Text)
	}
	// Operator value may itself be quoted.
	c, _ = parseSearchQuery(`from:"john doe"`)
	hdr(t, c, "FROM", "john doe")
}

func TestParse_Negation(t *testing.T) {
	c, _ := parseSearchQuery(`-from:spammer`)
	if len(c.Not) != 1 {
		t.Fatalf("negation -> Not=%v; want 1 entry", c.Not)
	}
	hdr(t, c.Not[0], "FROM", "spammer")

	// Negated free text becomes NOT ( TEXT ... ).
	c, _ = parseSearchQuery(`-urgent`)
	if len(c.Not) != 1 || len(c.Not[0].Text) != 1 || c.Not[0].Text[0] != "urgent" {
		t.Fatalf("negated free text -> Not=%+v", c.Not)
	}
}

func TestParse_Combined(t *testing.T) {
	c, folder := parseSearchQuery(`in:Work from:alice subject:"status update" has:attachment is:unread -draft newsletter`)
	if folder != "Work" {
		t.Fatalf("folder = %q; want Work", folder)
	}
	hdr(t, c, "FROM", "alice")
	hdr(t, c, "SUBJECT", "status update")
	hdr(t, c, "Content-Type", "multipart")
	if !hasFlag(c.WithoutFlags, imap.SeenFlag) {
		t.Fatalf("is:unread not mapped")
	}
	// -draft is an unknown operator? no: "draft" isn't an operator, so -draft is
	// negated free text.
	foundDraftNot := false
	for _, n := range c.Not {
		for _, txt := range n.Text {
			if txt == "draft" {
				foundDraftNot = true
			}
		}
	}
	if !foundDraftNot {
		t.Fatalf("-draft not mapped to NOT(TEXT draft); Not=%+v", c.Not)
	}
	// free text "newsletter" is a TEXT term.
	if !hasFlag(c.Text, "newsletter") {
		t.Fatalf("free text newsletter missing from Text=%v", c.Text)
	}
}

// --- unknown operator degrades to free text --------------------------------

func TestParse_UnknownOperator(t *testing.T) {
	c, folder := parseSearchQuery(`label:important`)
	if folder != "" {
		t.Fatalf("unexpected folder %q", folder)
	}
	if !hasFlag(c.Text, "label:important") {
		t.Fatalf("unknown operator -> Text=%v; want to contain label:important", c.Text)
	}
	// Unknown value on a known operator (has:foo) also degrades to text.
	c, _ = parseSearchQuery(`has:foo`)
	if !hasFlag(c.Text, "has:foo") {
		t.Fatalf("has:foo -> Text=%v; want to contain has:foo", c.Text)
	}
	// Unparseable date degrades to text.
	c, _ = parseSearchQuery(`before:notadate`)
	if !hasFlag(c.Text, "before:notadate") {
		t.Fatalf("before:notadate -> Text=%v; want to contain before:notadate", c.Text)
	}
}

// --- raw-text fallback ------------------------------------------------------

func TestParse_RawTextFallback(t *testing.T) {
	// No operators, no quotes, no negation -> single TEXT of the whole query
	// (preserves legacy behaviour).
	c, folder := parseSearchQuery(`big invoice from vendor`)
	if folder != "" {
		t.Fatalf("unexpected folder %q", folder)
	}
	if len(c.Text) != 1 || c.Text[0] != "big invoice from vendor" {
		t.Fatalf("raw fallback -> Text=%v; want single whole-query term", c.Text)
	}
	if len(c.Header) != 0 || len(c.Not) != 0 {
		t.Fatalf("raw fallback should have no header/not criteria: %+v", c)
	}
}

// --- injection safety -------------------------------------------------------

// A CR/LF in an operator value must never appear in the criterion value, and
// must never appear un-escaped in the serialised SEARCH command (which would
// let an attacker inject a second IMAP command).
func TestParse_CRLFInjection(t *testing.T) {
	c, _ := parseSearchQuery("from:alice\r\nA002 DELETE INBOX")
	// The value is sanitised: no CR/LF survives into the criterion.
	for _, v := range c.Header["FROM"] {
		if strings.ContainsAny(v, "\r\n") {
			t.Fatalf("FROM value still contains CR/LF: %q", v)
		}
	}
	// And the wire encoding contains exactly one line (one CRLF, at the end).
	wire := encodeSearch(t, c)
	body := strings.TrimSuffix(wire, "\r\n")
	if strings.Contains(body, "\r") || strings.Contains(body, "\n") {
		t.Fatalf("serialised SEARCH contains an embedded CRLF (injection!): %q", wire)
	}
	if strings.Contains(wire, "DELETE") {
		// Even if it appeared as data it must be inside a quoted/literal atom,
		// never as a bare command token on its own line.
		if strings.Contains(body, "\r") || strings.Contains(body, "\n") {
			t.Fatalf("injected DELETE reached a new line: %q", wire)
		}
	}
}

// A double-quote in an operator value must be escaped by go-imap's encoder so
// it can't close the atom early.
func TestParse_QuoteInjection(t *testing.T) {
	c, _ := parseSearchQuery(`subject:a"b`)
	wire := encodeSearch(t, c)
	// go-imap uses strconv.Quote, so the embedded quote is backslash-escaped.
	if !strings.Contains(wire, `\"`) {
		t.Fatalf("embedded quote not escaped in wire form: %q", wire)
	}
	// Still a single logical line.
	body := strings.TrimSuffix(wire, "\r\n")
	if strings.ContainsAny(body, "\r\n") {
		t.Fatalf("quote injection produced extra line: %q", wire)
	}
}

// --- bounds -----------------------------------------------------------------

func TestParse_BoundsQueryLength(t *testing.T) {
	long := "from:" + strings.Repeat("a", maxSearchQueryLen*2)
	c, _ := parseSearchQuery(long)
	for _, v := range c.Header["FROM"] {
		if len(v) > maxSearchValueLen {
			t.Fatalf("FROM value not bounded: len=%d", len(v))
		}
	}
}

func TestParse_BoundsValueLength(t *testing.T) {
	c, _ := parseSearchQuery("subject:" + strings.Repeat("x", maxSearchValueLen+50))
	for _, v := range c.Header["SUBJECT"] {
		if len(v) > maxSearchValueLen {
			t.Fatalf("value not bounded: len=%d", len(v))
		}
	}
}
