// handlers/api/democlient.go — in-memory mail client for demo / screenshot mode.
//
// DemoClient satisfies MailClient without making any network connections.
// It returns a fixed set of realistic seed messages seeded at construction
// time.  All mutating operations (delete, flag, save) are no-ops that succeed
// silently so the UI remains fully interactive during screenshotting.
//
// Activation: set [demo] enabled = true in config.toml.  The demo login
// route (POST /demo-login) accepts the demo username/password from config and
// creates a session that causes CreateIMAPClient to return a DemoClient.
package api

import (
	"fmt"
	"lilmail/models"
	"strings"
	"time"
)

// DemoClient is an in-memory MailClient backed by seed data.
type DemoClient struct {
	// inboxMessages are the seeded INBOX messages.
	inboxMessages []models.Email
	// sentMessages are the seeded Sent messages.
	sentMessages []models.Email
	// draftMessages are the seeded Draft messages.
	draftMessages []models.Email
}

// NewDemoClient constructs a DemoClient pre-loaded with realistic seed data.
func NewDemoClient() *DemoClient {
	now := time.Now()
	day := 24 * time.Hour

	inbox := []models.Email{
		{
			ID:        "1001",
			From:      "alice@example.com",
			FromName:  "Alice Nakamura",
			To:        "demo@lilmail.dev",
			Subject:   "Re: Product roadmap Q3 — feedback welcome",
			Preview:   "Thanks for sharing the draft. I left comments on sections 2 and 4. The timeline looks ambitious but achievable if we front-load the infra work.",
			Body:      "Thanks for sharing the draft. I left comments on sections 2 and 4.\n\nThe timeline looks ambitious but achievable if we front-load the infra work. Let's sync Thursday — does 14:00 UTC work for you?\n\n– Alice",
			HTML:      "<p>Thanks for sharing the draft. I left comments on sections 2 and 4.</p><p>The timeline looks ambitious but achievable if we front-load the infra work. Let's sync Thursday — does 14:00 UTC work for you?</p><p>– Alice</p>",
			Date:      now.Add(-2 * time.Hour),
			Flags:     []string{},
			MessageID: "<roadmap-reply-001@example.com>",
			InReplyTo: "<roadmap-001@lilmail.dev>",
			References: []string{
				"<roadmap-001@lilmail.dev>",
			},
		},
		{
			ID:             "1002",
			From:           "noreply@github.com",
			FromName:       "GitHub",
			To:             "demo@lilmail.dev",
			Subject:        "[lilmail/lilmail] PR #42: Add dark mode calendar view",
			Preview:        "imranparuk opened a pull request. Add a dark-mode-aware CSS layer for the calendar month view, fixing contrast issues on --color-scheme: dark.",
			Body:           "imranparuk opened a pull request #42\n\nAdd dark mode calendar view\n\nAdd a dark-mode-aware CSS layer for the calendar month view, fixing contrast issues on --color-scheme: dark.\n\nChanges: +187 −23\n\nView it on GitHub: https://github.com/lilmail/lilmail/pull/42",
			Date:           now.Add(-5 * time.Hour),
			Flags:          []string{"\\Seen"},
			MessageID:      "<github-pr-42@github.com>",
			HasAttachments: false,
		},
		{
			ID:             "1003",
			From:           "invoice@stripe.com",
			FromName:       "Stripe",
			To:             "demo@lilmail.dev",
			Subject:        "Your invoice from Stripe — $49.00 due",
			Preview:        "Invoice INV-2026-0614. Amount due: $49.00 USD. Due date: 30 June 2026. View and pay at dashboard.stripe.com.",
			Body:           "Invoice INV-2026-0614\n\nAmount due: $49.00 USD\nDue date: 30 June 2026\n\nView and pay at dashboard.stripe.com",
			Date:           now.Add(-18 * time.Hour),
			Flags:          []string{},
			MessageID:      "<invoice-2026-0614@stripe.com>",
			HasAttachments: true,
			Attachments: []models.Attachment{
				{
					ID:          "1003/1",
					Filename:    "invoice-INV-2026-0614.pdf",
					ContentType: "application/pdf",
					Size:        84320,
				},
			},
		},
		{
			ID:             "1004",
			From:           "bob@designco.io",
			FromName:       "Bob Osei",
			To:             "demo@lilmail.dev",
			Subject:        "Moodboard for the new landing page",
			Preview:        "Hey! Attached are three concept directions for the hero section. Let me know which resonates most — I'm leaning toward option B (the gradient mesh).",
			Body:           "Hey!\n\nAttached are three concept directions for the hero section. Let me know which resonates most — I'm leaning toward option B (the gradient mesh).\n\nAll exported at 2× for retina. LMK your thoughts by EOD Friday.\n\nCheers,\nBob",
			HTML:           "<p>Hey!</p><p>Attached are three concept directions for the hero section. Let me know which resonates most — I'm leaning toward option B (the gradient mesh).</p><p>All exported at 2× for retina. LMK your thoughts by EOD Friday.</p><p>Cheers,<br>Bob</p>",
			Date:           now.Add(-2 * day),
			Flags:          []string{"\\Seen"},
			MessageID:      "<moodboard-2026@designco.io>",
			HasAttachments: true,
			Attachments: []models.Attachment{
				{
					ID:          "1004/1",
					Filename:    "hero-concept-A.png",
					ContentType: "image/png",
					Size:        512000,
				},
				{
					ID:          "1004/2",
					Filename:    "hero-concept-B.png",
					ContentType: "image/png",
					Size:        489000,
				},
				{
					ID:          "1004/3",
					Filename:    "hero-concept-C.png",
					ContentType: "image/png",
					Size:        531000,
				},
			},
		},
		{
			ID:        "1005",
			From:      "alice@example.com",
			FromName:  "Alice Nakamura",
			To:        "demo@lilmail.dev",
			Subject:   "Product roadmap Q3 — feedback welcome",
			Preview:   "Hi team, attaching the Q3 roadmap draft. Please review sections 2–4 and share feedback by Friday. Ping me with any blockers.",
			Body:      "Hi team,\n\nAttaching the Q3 roadmap draft. Please review sections 2–4 and share feedback by Friday.\n\nPing me with any blockers.\n\n– Alice",
			Date:      now.Add(-3 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<roadmap-001@lilmail.dev>",
		},
		{
			ID:        "1006",
			From:      "security@accounts.google.com",
			FromName:  "Google",
			To:        "demo@lilmail.dev",
			Subject:   "Security alert: new sign-in on macOS",
			Preview:   "Your Google Account demo@gmail.com was just signed in to from macOS. If this was you, you can ignore this message.",
			Body:      "Your Google Account demo@gmail.com was just signed in to from macOS.\n\nIf this was you, you can ignore this message.\n\nIf not, visit myaccount.google.com/security to take action.",
			Date:      now.Add(-4 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<security-alert-1234@accounts.google.com>",
		},
		{
			ID:        "1007",
			From:      "team@linear.app",
			FromName:  "Linear",
			To:        "demo@lilmail.dev",
			Subject:   "ENG-419 was closed: Investigate IMAP IDLE reconnect drops",
			Preview:   "Issue ENG-419 — Investigate IMAP IDLE reconnect drops — was closed by imranparuk. View the issue on Linear.",
			Body:      "Issue ENG-419 — Investigate IMAP IDLE reconnect drops — was closed by imranparuk.\n\nView the issue: https://linear.app/team/ENG-419",
			Date:      now.Add(-5 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<linear-ENG-419-closed@linear.app>",
		},
		{
			ID:        "1008",
			From:      "maya@startup.co",
			FromName:  "Maya Chen",
			To:        "demo@lilmail.dev",
			Cc:        "team@startup.co",
			Subject:   "Onboarding call recap + next steps",
			Preview:   "Great call today! Recapping the key decisions: (1) launch date moved to July 14, (2) pricing stays at $29/mo for beta, (3) docs sprint starts Monday.",
			Body:      "Great call today! Recapping the key decisions:\n\n1. Launch date moved to July 14\n2. Pricing stays at $29/mo for beta\n3. Docs sprint starts Monday\n\nAction items:\n- Maya: finalize onboarding flow mockups by Wed\n- Demo: review billing integration PR\n- Team: update landing page copy\n\nTalk soon,\nMaya",
			HTML:      "<p>Great call today! Recapping the key decisions:</p><ol><li>Launch date moved to July 14</li><li>Pricing stays at $29/mo for beta</li><li>Docs sprint starts Monday</li></ol><p><strong>Action items:</strong></p><ul><li>Maya: finalize onboarding flow mockups by Wed</li><li>Demo: review billing integration PR</li><li>Team: update landing page copy</li></ul><p>Talk soon,<br>Maya</p>",
			Date:      now.Add(-6 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<onboarding-recap-001@startup.co>",
		},
		{
			ID:         "1009",
			From:       "alice@example.com",
			FromName:   "Alice Nakamura",
			To:         "demo@lilmail.dev",
			Subject:    "Re: Onboarding call recap + next steps",
			Preview:    "+1 to all of the above. I'll also chase down the SSO integration — should have a status update by end of week.",
			Body:       "+1 to all of the above. I'll also chase down the SSO integration — should have a status update by end of week.\n\n– Alice",
			Date:       now.Add(-6*day + 3*time.Hour),
			Flags:      []string{"\\Seen"},
			MessageID:  "<onboarding-recap-reply-001@example.com>",
			InReplyTo:  "<onboarding-recap-001@startup.co>",
			References: []string{"<onboarding-recap-001@startup.co>"},
		},
		{
			ID:        "1010",
			From:      "newsletter@techdigest.io",
			FromName:  "Tech Digest",
			To:        "demo@lilmail.dev",
			Subject:   "This week in open source: Go 1.24, the SFU debate, and HTMX hits 30k stars",
			Preview:   "Go 1.24 ships with range-over functions and improved PGO. The HTMX project crosses 30k GitHub stars. Plus: why SSE is back in fashion.",
			Body:      "Go 1.24 ships with range-over functions and improved PGO.\n\nThe HTMX project crosses 30k GitHub stars.\n\nPlus: why SSE is back in fashion for real-time web apps, a deep-dive on SFU vs mesh WebRTC, and the best new crates for Rust CLI tooling.",
			HTML:      "<div style=\"font-family:sans-serif\"><img src=\"https://techdigest.io/assets/header-banner.png\" alt=\"Tech Digest\" width=\"600\"><h2>This week in open source</h2><p><strong>Go 1.24</strong> ships with range-over functions and improved PGO.</p><p>The <strong>HTMX</strong> project crosses 30k GitHub stars.</p><p>Plus: why SSE is back in fashion for real-time web apps, a deep-dive on SFU vs mesh WebRTC, and the best new crates for Rust CLI tooling.</p><p><a href=\"https://techdigest.io/week24\">Read the full issue →</a></p></div>",
			Date:      now.Add(-7 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<techdigest-2026-week24@techdigest.io>",
		},
	}

	sent := []models.Email{
		{
			ID:        "2001",
			From:      "demo@lilmail.dev",
			FromName:  "Demo User",
			To:        "alice@example.com",
			Subject:   "Product roadmap Q3 — feedback welcome",
			Preview:   "Hi Alice, sharing the Q3 roadmap draft. Would love your thoughts on the timeline before we present to the board.",
			Body:      "Hi Alice,\n\nSharing the Q3 roadmap draft. Would love your thoughts on the timeline before we present to the board.\n\nThanks!",
			Date:      now.Add(-3 * day),
			Flags:     []string{"\\Seen"},
			MessageID: "<roadmap-sent-001@lilmail.dev>",
		},
		{
			ID:        "2002",
			From:      "demo@lilmail.dev",
			FromName:  "Demo User",
			To:        "bob@designco.io",
			Subject:   "Re: Moodboard for the new landing page",
			Preview:   "Hey Bob, option B all the way — the gradient mesh feels modern without being too trendy. Could you try a version where the mesh is slightly more muted?",
			Body:      "Hey Bob,\n\nOption B all the way — the gradient mesh feels modern without being too trendy.\n\nCould you try a version where the mesh is slightly more muted? Something that works in both light and dark mode would be ideal.\n\nThanks!",
			Date:      now.Add(-2*day + time.Hour),
			Flags:     []string{"\\Seen"},
			MessageID: "<moodboard-reply-001@lilmail.dev>",
		},
	}

	drafts := []models.Email{
		{
			ID:        "3001",
			From:      "demo@lilmail.dev",
			FromName:  "Demo User",
			To:        "team@startup.co",
			Subject:   "Sprint planning notes — week of June 16",
			Preview:   "Capturing the key points from today's planning. Still working through the acceptance criteria for ENG-42",
			Body:      "Capturing the key points from today's planning. Still working through the acceptance criteria for ENG-42...",
			Date:      now.Add(-30 * time.Minute),
			Flags:     []string{"\\Draft", "\\Seen"},
			MessageID: "<draft-sprint-001@lilmail.dev>",
		},
	}

	return &DemoClient{
		inboxMessages: inbox,
		sentMessages:  sent,
		draftMessages: drafts,
	}
}

// folderMessages returns the seed slice for the given folder name.
func (d *DemoClient) folderMessages(folderName string) []models.Email {
	lc := strings.ToLower(folderName)
	switch {
	case lc == "inbox":
		return d.inboxMessages
	case lc == "sent" || lc == "sent items" || lc == "sent mail":
		return d.sentMessages
	case lc == "drafts" || lc == "draft":
		return d.draftMessages
	default:
		return nil
	}
}

// FetchFolders returns a static folder list.
func (d *DemoClient) FetchFolders() ([]*MailboxInfo, error) {
	return []*MailboxInfo{
		{Name: "INBOX", Delimiter: "/", Attributes: []string{}, UnreadCount: 3},
		{Name: "Sent", Delimiter: "/", Attributes: []string{`\Sent`}},
		{Name: "Drafts", Delimiter: "/", Attributes: []string{`\Drafts`}},
		{Name: "Trash", Delimiter: "/", Attributes: []string{`\Trash`}},
	}, nil
}

// FetchMessages returns seed messages for the given folder (capped at limit).
func (d *DemoClient) FetchMessages(folderName string, limit uint32) ([]models.Email, error) {
	msgs := d.folderMessages(folderName)
	if uint32(len(msgs)) > limit {
		msgs = msgs[:limit]
	}
	return msgs, nil
}

// FetchSingleMessage returns the seed message matching uid in the given folder.
func (d *DemoClient) FetchSingleMessage(folderName, uid string) (models.Email, error) {
	msgs := d.folderMessages(folderName)
	for _, m := range msgs {
		if m.ID == uid {
			return m, nil
		}
	}
	// Also check across all folders (for unified/search paths).
	for _, m := range d.inboxMessages {
		if m.ID == uid {
			return m, nil
		}
	}
	for _, m := range d.sentMessages {
		if m.ID == uid {
			return m, nil
		}
	}
	return models.Email{}, fmt.Errorf("demo: message %s not found", uid)
}

// SearchMessages filters seed inbox messages whose Subject, From, or Body
// contains the query string (case-insensitive).
func (d *DemoClient) SearchMessages(folderName, query string, limit uint32) ([]models.Email, error) {
	q := strings.ToLower(query)
	var results []models.Email
	for _, m := range d.folderMessages(folderName) {
		if strings.Contains(strings.ToLower(m.Subject), q) ||
			strings.Contains(strings.ToLower(m.From), q) ||
			strings.Contains(strings.ToLower(m.Body), q) ||
			strings.Contains(strings.ToLower(m.Preview), q) {
			results = append(results, m)
			if limit > 0 && uint32(len(results)) >= limit {
				break
			}
		}
	}
	return results, nil
}

// FetchAttachment returns a minimal placeholder for the requested attachment.
func (d *DemoClient) FetchAttachment(folderName, uid, partPath string) ([]byte, string, string, error) {
	// Return a minimal 1-byte placeholder so the UI doesn't error.
	return []byte("(demo attachment)"), "attachment.bin", "application/octet-stream", nil
}

// DeleteMessage is a no-op in demo mode.
func (d *DemoClient) DeleteMessage(folderName, uid string) error { return nil }

// SetMessageFlag is a no-op in demo mode.
func (d *DemoClient) SetMessageFlag(folderName, uid string, flag string, add bool) error {
	return nil
}

// MoveMessage is a no-op in demo mode (the seed data is in-memory and immutable).
func (d *DemoClient) MoveMessage(srcFolder, uid, destFolder string) error { return nil }

// SaveToSent is a no-op in demo mode.
func (d *DemoClient) SaveToSent(to, subject, body string, rawMessage []byte) error { return nil }

// SaveDraft is a no-op in demo mode.
func (d *DemoClient) SaveDraft(rawMessage []byte) error { return nil }

// DeleteDraft is a no-op in demo mode.
func (d *DemoClient) DeleteDraft(uid string) error { return nil }

// DeleteMessageFromFolder is a no-op in demo mode.
func (d *DemoClient) DeleteMessageFromFolder(folder, uid string) error { return nil }

// DiscoverDraftsFolder returns the canonical demo drafts folder name.
func (d *DemoClient) DiscoverDraftsFolder() (string, error) { return "Drafts", nil }

// DiscoverTrashFolder returns the canonical demo trash folder name.
func (d *DemoClient) DiscoverTrashFolder() (string, error) { return "Trash", nil }

// WatchInbox is a no-op in demo mode; it blocks until stop is closed.
func (d *DemoClient) WatchInbox(stop <-chan struct{}, onNewMail func(email models.Email)) error {
	<-stop
	return nil
}

// Close is a no-op in demo mode (no network connection to close).
func (d *DemoClient) Close() error { return nil }
