package api

import (
	"strings"
	"testing"
	"time"

	"lilmail/models"
)

func sampleInvite() InviteParams {
	return InviteParams{
		Method:    "REQUEST",
		UID:       "evt-123@vulos",
		Sequence:  0,
		Organizer: "alice@example.com",
		OrgName:   "Alice",
		Summary:   "Sprint planning",
		Location:  "Room 1",
		Start:     time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC),
		Attendees: []models.Attendee{
			{Email: "bob@example.com", Name: "Bob"},
			{Email: "carol@example.com"},
		},
	}
}

func TestBuildRequestICS_RoundTrip(t *testing.T) {
	ics, err := BuildRequestICS(sampleInvite())
	if err != nil {
		t.Fatalf("BuildRequestICS: %v", err)
	}
	if !strings.Contains(ics, "METHOD:REQUEST") {
		t.Error("missing METHOD:REQUEST")
	}
	if !strings.Contains(ics, "ORGANIZER;CN=Alice:mailto:alice@example.com") {
		t.Errorf("organizer line missing/wrong:\n%s", ics)
	}
	if strings.Count(ics, "ATTENDEE") != 2 {
		t.Errorf("expected 2 ATTENDEE lines, got:\n%s", ics)
	}
	if !strings.Contains(ics, "PARTSTAT=NEEDS-ACTION") || !strings.Contains(ics, "RSVP=TRUE") {
		t.Error("attendee PARTSTAT/RSVP missing")
	}

	inv, err := ParseInvite([]byte(ics), "bob@example.com")
	if err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
	if inv == nil {
		t.Fatal("ParseInvite returned nil for a REQUEST")
	}
	if inv.Method != "REQUEST" || inv.UID != "evt-123@vulos" || inv.Summary != "Sprint planning" {
		t.Errorf("parsed fields wrong: %+v", inv)
	}
	if inv.Organizer != "alice@example.com" {
		t.Errorf("organizer parse = %q", inv.Organizer)
	}
	if len(inv.Attendees) != 2 {
		t.Errorf("attendees parse = %d", len(inv.Attendees))
	}
	if inv.MyPartStat != "NEEDS-ACTION" {
		t.Errorf("MyPartStat = %q, want NEEDS-ACTION", inv.MyPartStat)
	}
}

func TestEscICalText_NoInjection(t *testing.T) {
	p := sampleInvite()
	// A malicious summary attempting to inject a new property + PARTSTAT flip.
	p.Summary = "Evil\r\nX-INJECT:1;comma,semi;"
	ics, err := BuildRequestICS(p)
	if err != nil {
		t.Fatalf("BuildRequestICS: %v", err)
	}
	if strings.Contains(ics, "\r\nX-INJECT:1") {
		t.Errorf("newline injection not escaped:\n%s", ics)
	}
	// The escaped summary must appear on a single SUMMARY line.
	for _, line := range strings.Split(ics, "\r\n") {
		if strings.HasPrefix(line, "SUMMARY:") {
			if !strings.Contains(line, "\\n") || !strings.Contains(line, "\\;") {
				t.Errorf("summary not escaped: %q", line)
			}
		}
	}
	// It must still parse as valid iCalendar.
	if _, err := ParseInvite([]byte(ics), ""); err != nil {
		t.Fatalf("escaped ics failed to parse: %v", err)
	}
}

func TestBuildReplyICS(t *testing.T) {
	ics, err := BuildReplyICS(ReplyParams{
		UID:       "evt-123@vulos",
		Organizer: "alice@example.com",
		Attendee:  "bob@example.com",
		PartStat:  "ACCEPTED",
		Summary:   "Sprint planning",
	})
	if err != nil {
		t.Fatalf("BuildReplyICS: %v", err)
	}
	if !strings.Contains(ics, "METHOD:REPLY") {
		t.Error("missing METHOD:REPLY")
	}
	if !strings.Contains(ics, "ATTENDEE;PARTSTAT=ACCEPTED:mailto:bob@example.com") {
		t.Errorf("reply attendee line wrong:\n%s", ics)
	}

	inv, err := ParseInvite([]byte(ics), "bob@example.com")
	if err != nil || inv == nil {
		t.Fatalf("ParseInvite reply: %v", err)
	}
	if inv.Method != "REPLY" || len(inv.Attendees) != 1 || inv.Attendees[0].PartStat != "ACCEPTED" {
		t.Errorf("parsed reply wrong: %+v", inv)
	}
}

func TestBuildReplyICS_BadPartStat(t *testing.T) {
	if _, err := BuildReplyICS(ReplyParams{UID: "x", Attendee: "b@x.com", PartStat: "MAYBE"}); err == nil {
		t.Error("expected error for invalid PARTSTAT")
	}
}

func TestBuildRequestICS_AttendeeCap(t *testing.T) {
	p := sampleInvite()
	p.Attendees = make([]models.Attendee, MaxAttendees+1)
	for i := range p.Attendees {
		p.Attendees[i] = models.Attendee{Email: "a@example.com"}
	}
	if _, err := BuildRequestICS(p); err == nil {
		t.Error("expected error when exceeding MaxAttendees")
	}
}

func TestBuildIMIPMessage_Structure(t *testing.T) {
	ics, _ := BuildRequestICS(sampleInvite())
	raw, err := BuildIMIPMessage("alice@example.com", "Alice", "bob@example.com",
		"Invitation: Sprint planning", "You are invited.", ics, "REQUEST")
	if err != nil {
		t.Fatalf("BuildIMIPMessage: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/alternative") {
		t.Error("not multipart/alternative")
	}
	if !strings.Contains(s, "text/calendar") || !strings.Contains(s, "method=REQUEST") {
		t.Errorf("missing text/calendar; method=REQUEST part:\n%s", s)
	}
	if !strings.Contains(s, "To: bob@example.com") {
		t.Error("missing To header")
	}
}

func TestBuildIMIPMessage_HeaderInjection(t *testing.T) {
	ics, _ := BuildRequestICS(sampleInvite())
	if _, err := BuildIMIPMessage("alice@example.com", "Alice",
		"bob@example.com\r\nBcc: victim@example.com", "Subj", "body", ics, "REQUEST"); err == nil {
		t.Error("expected header-injection rejection in To")
	}
}

func TestExtractCalendarPart_FromIMIP(t *testing.T) {
	ics, _ := BuildRequestICS(sampleInvite())
	raw, err := BuildIMIPMessage("alice@example.com", "Alice", "bob@example.com",
		"Invitation", "You are invited.", ics, "REQUEST")
	if err != nil {
		t.Fatalf("BuildIMIPMessage: %v", err)
	}
	got := extractCalendarPart(raw)
	if got == nil {
		t.Fatal("extractCalendarPart returned nil for an iMIP message")
	}
	inv, err := ParseInvite(got, "bob@example.com")
	if err != nil || inv == nil {
		t.Fatalf("round-trip parse failed: %v", err)
	}
	if inv.Method != "REQUEST" || inv.UID != "evt-123@vulos" {
		t.Errorf("extracted invite wrong: %+v", inv)
	}
}

func TestExtractCalendarPart_None(t *testing.T) {
	raw := "From: a@x.com\r\nTo: b@x.com\r\nSubject: hi\r\n" +
		"Content-Type: text/plain\r\n\r\njust a normal email\r\n"
	if got := extractCalendarPart([]byte(raw)); got != nil {
		t.Errorf("expected nil for plain email, got %q", got)
	}
}

func TestParseInvite_NonSchedulingReturnsNil(t *testing.T) {
	// A VCALENDAR with no METHOD is a plain publish/export, not a scheduling msg.
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:1\r\n" +
		"DTSTAMP:20260101T000000Z\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR"
	inv, err := ParseInvite([]byte(ics), "")
	if err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
	if inv != nil {
		t.Errorf("expected nil for non-scheduling calendar, got %+v", inv)
	}
}
