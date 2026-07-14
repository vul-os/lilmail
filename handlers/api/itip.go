// handlers/api/itip.go — iTIP/iMIP calendar-invite engine (RFC 5545 / 5546 / 6047).
//
// This is the single source of truth for generating and parsing calendar
// scheduling messages in LilMail:
//
//   - BuildRequestICS  → a METHOD:REQUEST VCALENDAR an organizer mails to invitees
//   - BuildReplyICS    → a METHOD:REPLY VCALENDAR an attendee mails back on RSVP
//   - ParseInvite      → decode an inbound text/calendar part into models.CalendarInvite
//   - BuildIMIPMessage → wrap an ICS body as a proper iMIP MIME message
//     (multipart/alternative: text/plain human summary + text/calendar; method=…)
//
// iCalendar text values are escaped per RFC 5545 §3.3.11 (escICalText) so a
// summary/description/location cannot inject extra properties or fold breaks —
// this is the calendar analogue of header-injection hardening. Values that flow
// into MIME/RFC 5322 headers (addresses, boundaries) are validated separately.
package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"

	"lilmail/models"

	"github.com/emersion/go-ical"
)

// MaxAttendees caps the invitee count on a single event. Invites are only ever
// generated for attendees the user put on their OWN event (never harvested from
// arbitrary mail), but the cap is a second line of defence against turning the
// send path into a fan-out/spam vector.
const MaxAttendees = 100

// iCalProdID identifies LilMail as the generating product in every VCALENDAR.
const iCalProdID = "-//Vulos//LilMail iTIP//EN"

// escICalText escapes a text value for safe inclusion as an iCalendar property
// value per RFC 5545 §3.3.11: backslash, semicolon and comma are escaped, and
// CR/LF are turned into the literal "\n" escape. This prevents a crafted
// summary/description from injecting a new property line (the iCal equivalent of
// header injection). Content lines are NOT folded here — go-ical's encoder folds
// on output; the hand-rolled builders below keep values short and rely on this
// escaping for correctness.
func escICalText(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		";", "\\;",
		",", "\\,",
		"\r\n", "\\n",
		"\n", "\\n",
		"\r", "\\n",
	)
	return r.Replace(s)
}

// sanitizeAddr strips a mailto: prefix and any CR/LF (defence against header
// injection) from an email address and lower-cases the host part loosely by
// trimming. It returns the bare address.
func sanitizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "MAILTO:")
	addr = strings.TrimPrefix(addr, "mailto:")
	addr = strings.ReplaceAll(addr, "\r", "")
	addr = strings.ReplaceAll(addr, "\n", "")
	return strings.TrimSpace(addr)
}

// icalUTC formats t as an iCalendar UTC date-time (form "20060102T150405Z").
func icalUTC(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// icalDate formats t as an iCalendar DATE value (form "20060102").
func icalDate(t time.Time) string {
	return t.Format("20060102")
}

// InviteParams carries everything needed to build a REQUEST/CANCEL invite.
type InviteParams struct {
	Method      string // "REQUEST" or "CANCEL"
	UID         string
	Sequence    int
	Organizer   string // bare email of the organizer
	OrgName     string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	Recurrence  string // raw RRULE value, optional
	Attendees   []models.Attendee
}

// BuildRequestICS builds a METHOD:REQUEST (or CANCEL) VCALENDAR body ready to be
// wrapped in an iMIP message. It writes ORGANIZER and one ATTENDEE line per
// invitee with RSVP=TRUE and the attendee's PARTSTAT (defaulting NEEDS-ACTION).
// All text values are RFC 5545-escaped. Attendee count is capped at MaxAttendees.
func BuildRequestICS(p InviteParams) (string, error) {
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method != "REQUEST" && method != "CANCEL" {
		return "", fmt.Errorf("itip: unsupported method %q", p.Method)
	}
	if p.UID == "" {
		return "", fmt.Errorf("itip: UID is required")
	}
	org := sanitizeAddr(p.Organizer)
	if org == "" {
		return "", fmt.Errorf("itip: organizer is required")
	}
	if len(p.Attendees) > MaxAttendees {
		return "", fmt.Errorf("itip: too many attendees (%d > %d)", len(p.Attendees), MaxAttendees)
	}

	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteString("\r\n") }

	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:" + iCalProdID)
	w("CALSCALE:GREGORIAN")
	w("METHOD:" + method)
	w("BEGIN:VEVENT")
	w("UID:" + escICalText(p.UID))
	w("DTSTAMP:" + icalUTC(time.Now()))
	w(fmt.Sprintf("SEQUENCE:%d", p.Sequence))

	if p.AllDay {
		w("DTSTART;VALUE=DATE:" + icalDate(p.Start))
		w("DTEND;VALUE=DATE:" + icalDate(p.End))
	} else {
		w("DTSTART:" + icalUTC(p.Start))
		w("DTEND:" + icalUTC(p.End))
	}

	if s := strings.TrimSpace(p.Summary); s != "" {
		w("SUMMARY:" + escICalText(s))
	}
	if s := strings.TrimSpace(p.Description); s != "" {
		w("DESCRIPTION:" + escICalText(s))
	}
	if s := strings.TrimSpace(p.Location); s != "" {
		w("LOCATION:" + escICalText(s))
	}
	if s := strings.TrimSpace(p.Recurrence); s != "" {
		// RRULE is structured; escape only CR/LF to avoid line injection.
		clean := strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "")
		w("RRULE:" + clean)
	}

	// ORGANIZER
	if p.OrgName != "" {
		w("ORGANIZER;CN=" + escICalParam(p.OrgName) + ":mailto:" + org)
	} else {
		w("ORGANIZER:mailto:" + org)
	}

	// ATTENDEEs
	for _, a := range p.Attendees {
		addr := sanitizeAddr(a.Email)
		if addr == "" {
			continue
		}
		part := a.PartStat
		if part == "" {
			part = "NEEDS-ACTION"
		}
		role := a.Role
		if role == "" {
			role = "REQ-PARTICIPANT"
		}
		line := "ATTENDEE;ROLE=" + role + ";PARTSTAT=" + part + ";RSVP=TRUE"
		if a.Name != "" {
			line += ";CN=" + escICalParam(a.Name)
		}
		line += ":mailto:" + addr
		w(line)
	}

	if method == "CANCEL" {
		w("STATUS:CANCELLED")
	}

	w("END:VEVENT")
	w("END:VCALENDAR")
	return b.String(), nil
}

// escICalParam escapes a value used inside an iCalendar parameter (e.g. CN).
// Parameter values may not contain the control characters or a bare double
// quote; per RFC 5545 §3.2 a value containing ':', ';' or ',' must be quoted.
// We strip quotes/newlines and quote when needed.
func escICalParam(s string) string {
	s = strings.ReplaceAll(s, "\"", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if strings.ContainsAny(s, ":;,") {
		return "\"" + s + "\""
	}
	return s
}

// ReplyParams carries everything needed to build a METHOD:REPLY.
type ReplyParams struct {
	UID          string
	Sequence     int
	Organizer    string // bare email of the original organizer
	Attendee     string // bare email of the responding attendee
	AttendeeName string
	PartStat     string // ACCEPTED, DECLINED, TENTATIVE
	Summary      string // echoed for a friendlier organizer-side display
}

// BuildReplyICS builds a METHOD:REPLY VCALENDAR the responding attendee mails
// back to the organizer, carrying the single ATTENDEE line with the chosen
// PARTSTAT. RFC 5546 requires echoing UID, ORGANIZER and the responding ATTENDEE.
func BuildReplyICS(p ReplyParams) (string, error) {
	if p.UID == "" {
		return "", fmt.Errorf("itip: reply requires UID")
	}
	att := sanitizeAddr(p.Attendee)
	if att == "" {
		return "", fmt.Errorf("itip: reply requires attendee address")
	}
	part := strings.ToUpper(strings.TrimSpace(p.PartStat))
	switch part {
	case "ACCEPTED", "DECLINED", "TENTATIVE":
	default:
		return "", fmt.Errorf("itip: invalid PARTSTAT %q", p.PartStat)
	}
	org := sanitizeAddr(p.Organizer)

	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteString("\r\n") }

	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:" + iCalProdID)
	w("CALSCALE:GREGORIAN")
	w("METHOD:REPLY")
	w("BEGIN:VEVENT")
	w("UID:" + escICalText(p.UID))
	w("DTSTAMP:" + icalUTC(time.Now()))
	w(fmt.Sprintf("SEQUENCE:%d", p.Sequence))
	if s := strings.TrimSpace(p.Summary); s != "" {
		w("SUMMARY:" + escICalText(s))
	}
	if org != "" {
		w("ORGANIZER:mailto:" + org)
	}
	line := "ATTENDEE;PARTSTAT=" + part
	if p.AttendeeName != "" {
		line += ";CN=" + escICalParam(p.AttendeeName)
	}
	line += ":mailto:" + att
	w(line)
	w("END:VEVENT")
	w("END:VCALENDAR")
	return b.String(), nil
}

// BuildIMIPMessage assembles a complete RFC 5322 message carrying an iMIP
// (RFC 6047) calendar part. The structure is:
//
//	multipart/alternative
//	  ├─ text/plain               (human-readable summary)
//	  └─ text/calendar; method=…  (the ICS, also offered as an .ics attachment
//	                               via Content-Disposition for wide client support)
//
// `method` is the iTIP method (REQUEST/REPLY/CANCEL); it is echoed in the part's
// Content-Type parameter as required by RFC 6047 §2.4. Addresses are validated
// for CR/LF before being placed in headers.
func BuildIMIPMessage(from, fromName, to, subject, plainBody, ics, method string) ([]byte, error) {
	from = sanitizeAddr(from)
	if from == "" {
		return nil, fmt.Errorf("imip: from is required")
	}
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return nil, fmt.Errorf("imip: header injection detected")
	}
	method = strings.ToUpper(strings.TrimSpace(method))

	msgID := fmt.Sprintf("<%s@%s>", generateMsgID(), GetDomainFromEmail(from))
	now := time.Now().Format(time.RFC822Z)

	var out bytes.Buffer
	boundary := generateBoundary()

	// Top-level headers.
	out.WriteString("Date: " + now + "\r\n")
	if fromName != "" {
		out.WriteString("From: " + mime.QEncoding.Encode("utf-8", fromName) + " <" + from + ">\r\n")
	} else {
		out.WriteString("From: " + from + "\r\n")
	}
	out.WriteString("To: " + to + "\r\n")
	out.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	out.WriteString("Message-ID: " + msgID + "\r\n")
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	out.WriteString("\r\n")

	mw := multipart.NewWriter(&out)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("imip: set boundary: %w", err)
	}

	// Part 1: text/plain human summary.
	if plainBody == "" {
		plainBody = subject
	}
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Type", "text/plain; charset=\"utf-8\"")
	ph.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := mw.CreatePart(ph)
	if err != nil {
		return nil, err
	}
	qp := quotedprintable.NewWriter(pw)
	if _, err := qp.Write([]byte(plainBody)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}

	// Part 2: text/calendar; method=… (base64 to survive transport intact).
	ch := textproto.MIMEHeader{}
	ch.Set("Content-Type", "text/calendar; charset=\"utf-8\"; method="+method+"; component=VEVENT")
	ch.Set("Content-Transfer-Encoding", "base64")
	ch.Set("Content-Disposition", "attachment; filename=\"invite.ics\"")
	cw, err := mw.CreatePart(ch)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString([]byte(ics))
	for len(enc) > 76 {
		if _, err := cw.Write([]byte(enc[:76] + "\r\n")); err != nil {
			return nil, err
		}
		enc = enc[76:]
	}
	if len(enc) > 0 {
		if _, err := cw.Write([]byte(enc + "\r\n")); err != nil {
			return nil, err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ParseInvite decodes a raw text/calendar body (already transfer-decoded) into a
// models.CalendarInvite. viewer is the email of the user viewing the message; it
// is used to populate MyPartStat from the attendee list. Returns (nil, nil) when
// the body is not a schedulable iTIP object (no METHOD, or no VEVENT).
func ParseInvite(ics []byte, viewer string) (*models.CalendarInvite, error) {
	dec := ical.NewDecoder(bytes.NewReader(ics))
	cal, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("itip: decode: %w", err)
	}

	method, _ := cal.Props.Text(ical.PropMethod)
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		// Not a scheduling message (e.g. a plain published calendar export).
		return nil, nil
	}

	events := cal.Events()
	if len(events) == 0 {
		return nil, nil
	}
	ev := events[0]

	inv := &models.CalendarInvite{Method: method}
	inv.UID, _ = ev.Props.Text(ical.PropUID)
	inv.Summary, _ = ev.Props.Text(ical.PropSummary)
	inv.Description, _ = ev.Props.Text(ical.PropDescription)
	inv.Location, _ = ev.Props.Text(ical.PropLocation)

	if org := ev.Props.Get(ical.PropOrganizer); org != nil {
		inv.Organizer = sanitizeAddr(org.Value)
	}
	if rr := ev.Props.Get(ical.PropRecurrenceRule); rr != nil {
		inv.Recurrence = rr.Value
	}
	if seq := ev.Props.Get(ical.PropSequence); seq != nil {
		if n, err := seq.Int(); err == nil {
			inv.Sequence = n
		}
	}

	if start, err := ev.DateTimeStart(time.Local); err == nil {
		inv.Start = start
	}
	if end, err := ev.DateTimeEnd(time.Local); err == nil {
		inv.End = end
	} else {
		inv.End = inv.Start
	}
	if dts := ev.Props.Get(ical.PropDateTimeStart); dts != nil {
		inv.AllDay = dts.ValueType() == ical.ValueDate
	}

	viewer = strings.ToLower(sanitizeAddr(viewer))
	for _, prop := range ev.Props.Values(ical.PropAttendee) {
		addr := sanitizeAddr(prop.Value)
		if addr == "" {
			continue
		}
		a := models.Attendee{Email: addr}
		if cn := prop.Params.Get(ical.ParamParticipationStatus); cn != "" {
			a.PartStat = strings.ToUpper(cn)
		}
		if role := prop.Params.Get(ical.ParamRole); role != "" {
			a.Role = role
		}
		if cn := prop.Params.Get(ical.ParamCommonName); cn != "" {
			a.Name = cn
		}
		inv.Attendees = append(inv.Attendees, a)
		if viewer != "" && strings.ToLower(addr) == viewer {
			inv.MyPartStat = a.PartStat
		}
	}

	return inv, nil
}
