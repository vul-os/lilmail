package api

import (
	"strings"
	"testing"
	"time"

	"lilmail/models"
)

// itip_e2e_test.go — end-to-end RSVP happy path for the calendar-invite surface
// (Wave-39). It exercises the full receive→respond seam at the engine layer:
//
//	organizer builds REQUEST iMIP  →  attendee extracts + parses it (RSVP card
//	inputs)  →  attendee builds an ACCEPTED REPLY iMIP  →  the reply is well-formed,
//	carries PARTSTAT=ACCEPTED, names the responder, and targets the real organizer.
func TestRSVP_HappyPath_E2E(t *testing.T) {
	organizer := "alice@example.com"
	attendee := "bob@example.com"

	// 1) Organizer sends a METHOD:REQUEST iMIP invite to the attendee.
	reqICS, err := BuildRequestICS(InviteParams{
		Method:    "REQUEST",
		UID:       "evt-e2e@vulos",
		Sequence:  0,
		Organizer: organizer,
		OrgName:   "Alice",
		Summary:   "Sprint planning",
		Location:  "Room 1",
		Start:     time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC),
		Attendees: []models.Attendee{{Email: attendee, Name: "Bob"}},
	})
	if err != nil {
		t.Fatalf("BuildRequestICS: %v", err)
	}
	inviteMsg, err := BuildIMIPMessage(organizer, "Alice", attendee,
		"Invitation: Sprint planning", "You are invited.", reqICS, "REQUEST")
	if err != nil {
		t.Fatalf("BuildIMIPMessage(REQUEST): %v", err)
	}

	// 2) Attendee's client receives the raw mail: extract + parse the invite. This
	// is exactly what feeds the RSVP card (models.CalendarInvite).
	cal := extractCalendarPart(inviteMsg)
	if cal == nil {
		t.Fatal("extractCalendarPart returned nil for a REQUEST iMIP message")
	}
	inv, err := ParseInvite(cal, attendee)
	if err != nil || inv == nil {
		t.Fatalf("ParseInvite: err=%v inv=%v", err, inv)
	}
	if inv.Method != "REQUEST" {
		t.Errorf("method = %q, want REQUEST", inv.Method)
	}
	if inv.UID != "evt-e2e@vulos" || inv.Summary != "Sprint planning" {
		t.Errorf("parsed invite fields wrong: %+v", inv)
	}
	if inv.Organizer != organizer {
		t.Errorf("parsed organizer = %q, want %q", inv.Organizer, organizer)
	}
	if inv.MyPartStat != "NEEDS-ACTION" {
		t.Errorf("MyPartStat = %q, want NEEDS-ACTION (RSVP card shows unanswered)", inv.MyPartStat)
	}

	// 3) Attendee clicks Accept. The RSVP endpoint derives the attendee from the
	// authenticated session (NOT the invite), and builds a METHOD:REPLY to the
	// organizer parsed from the invite.
	replyICS, err := BuildReplyICS(ReplyParams{
		UID:          inv.UID,
		Sequence:     inv.Sequence,
		Organizer:    inv.Organizer, // real organizer from the invite
		Attendee:     attendee,      // server identity
		AttendeeName: "Bob",
		PartStat:     "ACCEPTED",
		Summary:      inv.Summary,
	})
	if err != nil {
		t.Fatalf("BuildReplyICS: %v", err)
	}
	if !strings.Contains(replyICS, "METHOD:REPLY") {
		t.Error("reply missing METHOD:REPLY")
	}
	if !strings.Contains(replyICS, "ATTENDEE;PARTSTAT=ACCEPTED") {
		t.Errorf("reply missing PARTSTAT=ACCEPTED:\n%s", replyICS)
	}
	if !strings.Contains(replyICS, "mailto:"+attendee) {
		t.Errorf("reply does not name the responder:\n%s", replyICS)
	}

	// 4) The REPLY iMIP mail must be addressed to the real organizer.
	replyMsg, err := BuildIMIPMessage(attendee, "Bob", inv.Organizer,
		"Accepted: Sprint planning", "Bob accepted.", replyICS, "REPLY")
	if err != nil {
		t.Fatalf("BuildIMIPMessage(REPLY): %v", err)
	}
	rs := string(replyMsg)
	if !strings.Contains(rs, "To: "+organizer) {
		t.Errorf("reply not addressed to organizer:\n%s", rs)
	}
	if !strings.Contains(rs, "method=REPLY") {
		t.Error("reply mail missing method=REPLY calendar part")
	}

	// 5) Round-trip the reply back through the parser (organizer-side view): the
	// organizer's client should see Bob = ACCEPTED.
	replyCal := extractCalendarPart(replyMsg)
	if replyCal == nil {
		t.Fatal("could not extract calendar from REPLY mail")
	}
	parsedReply, err := ParseInvite(replyCal, organizer)
	if err != nil || parsedReply == nil {
		t.Fatalf("parse reply: err=%v", err)
	}
	if parsedReply.Method != "REPLY" {
		t.Errorf("reply method = %q", parsedReply.Method)
	}
	if len(parsedReply.Attendees) != 1 || parsedReply.Attendees[0].Email != attendee {
		t.Fatalf("reply attendees = %+v, want [%s]", parsedReply.Attendees, attendee)
	}
	if parsedReply.Attendees[0].PartStat != "ACCEPTED" {
		t.Errorf("reply PARTSTAT = %q, want ACCEPTED", parsedReply.Attendees[0].PartStat)
	}
}
