package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"lilmail/models"
)

// itip_security_test.go — red-team regression tests for the UNTRUSTED inbound
// calendar-invite parse path (Wave-39 calendar security review). The MX feeds
// attacker-controlled text/calendar straight into ParseInvite/extractCalendarPart,
// so these prove: no panic on hostile input, no property/header injection into an
// outbound REPLY, and bounded work on pathological MIME.

// TestParseInvite_MalformedNoPanic feeds a battery of malformed / truncated /
// oversized iCalendar bodies. Parsing must fail closed (nil/err), never panic.
func TestParseInvite_MalformedNoPanic(t *testing.T) {
	inputs := map[string][]byte{
		"empty":          []byte(""),
		"just-begin":     []byte("BEGIN:VCALENDAR"),
		"binary-garbage": []byte("garbage\x00\x01\x02\xff\xfe"),
		"unterminated":   []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n"),
		"bad-dtstart":    []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nDTSTART:notadate\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"),
		"empty-attendee": []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nATTENDEE:mailto:\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"),
		"only-crlf":      []byte("\r\n\r\n\r\n"),
		"huge-flat":      []byte(strings.Repeat("A:B\r\n", 100000)),
		"nested-vcal":    []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nEND:VEVENT\r\nEND:VCALENDAR\r\nEND:VCALENDAR\r\n"),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseInvite panicked on %q: %v", name, r)
				}
			}()
			_, _ = ParseInvite(in, "me@example.com")
		})
	}
}

// TestParseInvite_NoInjectionIntoReply proves that a hostile ORGANIZER / SUMMARY
// carrying CRLF + a smuggled extra property cannot forge a new property line when
// the parsed values are later echoed into an outbound METHOD:REPLY (the RSVP path).
func TestParseInvite_NoInjectionIntoReply(t *testing.T) {
	hostile := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:evil@x\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260101T000000Z\r\n" +
		"SUMMARY:Hi\\nX-EVIL:1\r\n" + // literal escaped newline the attacker hopes to un-escape
		"ORGANIZER:mailto:real-organizer@victim.test\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:me@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	inv, err := ParseInvite([]byte(hostile), "me@example.com")
	if err != nil || inv == nil {
		t.Fatalf("parse hostile invite: err=%v inv=%v", err, inv)
	}
	// The reply the RSVP endpoint would build: attendee is SERVER-derived, never
	// taken from the invite; organizer + summary are echoed from the invite.
	ics, err := BuildReplyICS(ReplyParams{
		UID:          inv.UID,
		Organizer:    inv.Organizer,
		Attendee:     "me@example.com", // server identity, not attacker-chosen
		AttendeeName: "me",
		PartStat:     "ACCEPTED",
		Summary:      inv.Summary,
	})
	if err != nil {
		t.Fatalf("BuildReplyICS: %v", err)
	}
	// No raw injected property line may appear anywhere in the reply.
	if strings.Contains(ics, "\r\nX-EVIL:1") {
		t.Errorf("property injection survived into REPLY:\n%s", ics)
	}
	// Exactly one SUMMARY line and one ATTENDEE line (the responder only).
	if n := strings.Count(ics, "\r\nSUMMARY:"); n != 1 {
		t.Errorf("expected 1 SUMMARY line, got %d:\n%s", n, ics)
	}
	if n := strings.Count(ics, "\r\nATTENDEE"); n != 1 {
		t.Errorf("expected 1 ATTENDEE line in reply, got %d:\n%s", n, ics)
	}
	// The reply must target the real organizer, not an attacker-influenced one.
	if !strings.Contains(ics, "ORGANIZER:mailto:real-organizer@victim.test") {
		t.Errorf("reply organizer wrong:\n%s", ics)
	}
}

// TestBuildRequestICS_HostileCNNoBreakout proves a malicious attendee CN (which
// in the SEND path comes from the user's OWN event editor) cannot break out of
// its quoted iCal parameter to forge a second routable mailto or inject a line.
func TestBuildRequestICS_HostileCNNoBreakout(t *testing.T) {
	p := sampleInvite()
	p.Attendees = []models.Attendee{
		{Email: "bob@example.com", Name: `Bob";RSVP=TRUE:mailto:victim@x.test` + "\r\nX-INJECT:1"},
	}
	ics, err := BuildRequestICS(p)
	if err != nil {
		t.Fatalf("BuildRequestICS: %v", err)
	}
	if strings.Contains(ics, "\r\nX-INJECT:1") {
		t.Errorf("CRLF in CN injected a new line:\n%s", ics)
	}
	// Round-trip: the hostile CN must resolve to exactly ONE attendee (bob), and
	// victim@x.test must NOT appear as a routable attendee address.
	inv, err := ParseInvite([]byte(ics), "")
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(inv.Attendees) != 1 {
		t.Fatalf("hostile CN produced %d attendees (breakout), want 1: %+v", len(inv.Attendees), inv.Attendees)
	}
	if inv.Attendees[0].Email != "bob@example.com" {
		t.Errorf("attendee email = %q, want bob@example.com", inv.Attendees[0].Email)
	}
	if strings.Contains(inv.Attendees[0].Email, "victim") {
		t.Error("victim address became a routable attendee")
	}
}

// TestExtractCalendarPart_BoundedDepth proves the recursive MIME walk fails closed
// on pathological nesting (billion-laughs-style) rather than exhausting the stack
// or hanging: beyond the depth guard it returns nil in bounded time.
func TestExtractCalendarPart_BoundedDepth(t *testing.T) {
	build := func(depth int) []byte {
		var sb strings.Builder
		sb.WriteString("From: a@b\r\nMIME-Version: 1.0\r\n")
		for i := 0; i < depth; i++ {
			b := fmt.Sprintf("B%d", i)
			nb := fmt.Sprintf("B%d", i+1)
			if i == 0 {
				sb.WriteString("Content-Type: multipart/mixed; boundary=\"" + b + "\"\r\n\r\n")
			}
			sb.WriteString("--" + b + "\r\nContent-Type: multipart/mixed; boundary=\"" + nb + "\"\r\n\r\n")
		}
		sb.WriteString("--B" + fmt.Sprint(depth) + "\r\nContent-Type: text/calendar\r\n\r\n")
		sb.WriteString("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
		return []byte(sb.String())
	}
	done := make(chan bool, 1)
	go func() {
		defer func() { _ = recover(); done <- true }()
		// Depth 50 is beyond the depth-10 guard: must return nil (fail closed).
		if got := extractCalendarPart(build(50)); got != nil {
			t.Errorf("expected nil beyond depth guard, got a calendar part")
		}
		// A calendar within the guard is still found (typical iMIP nesting:
		// multipart/mixed › multipart/alternative › text/calendar).
		within := "From: a@b\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"OUT\"\r\n\r\n" +
			"--OUT\r\nContent-Type: multipart/alternative; boundary=\"IN\"\r\n\r\n" +
			"--IN\r\nContent-Type: text/plain\r\n\r\nyou are invited\r\n" +
			"--IN\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" +
			"BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n" +
			"--IN--\r\n--OUT--\r\n"
		if got := extractCalendarPart([]byte(within)); got == nil {
			t.Error("failed to find a calendar within the depth guard")
		}
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("extractCalendarPart did not terminate on deep nesting (unbounded work)")
	}
}

// TestExtractCalendarPart_WideBounded proves a very wide (many-sibling-parts) MIME
// message is walked in bounded time without a calendar part being present.
func TestExtractCalendarPart_WideBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("From: a@b\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"X\"\r\n\r\n")
	for i := 0; i < 50000; i++ {
		sb.WriteString("--X\r\nContent-Type: text/plain\r\n\r\nn\r\n")
	}
	sb.WriteString("--X--\r\n")
	start := time.Now()
	if got := extractCalendarPart([]byte(sb.String())); got != nil {
		t.Error("unexpected calendar part in wide message")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("wide MIME walk too slow: %v", el)
	}
}
