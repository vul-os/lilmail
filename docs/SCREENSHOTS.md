# Screenshots

This document describes the screenshot gallery, how to regenerate screenshots,
and the two modes available: demo mode (no credentials required) and live IMAP
mode.

## Gallery

| File | Description | Status |
|------|-------------|--------|
| `docs/screenshots/hero.png` | Inbox hero shot (demo data) | Real — demo mode |
| `docs/screenshots/login.png` | Login page | Real — no credentials needed |
| `docs/screenshots/inbox.png` | Inbox with seeded demo messages | Real — demo mode |
| `docs/screenshots/message.png` | Message viewer | Real — demo mode |
| `docs/screenshots/compose.png` | Compose modal with CC/BCC and attachment UI | Real — demo mode |
| `docs/screenshots/search.png` | Search results filtered by query | Real — demo mode |
| `docs/screenshots/calendar.png` | Calendar month view | Needs CalDAV (`[caldav]` enabled) |
| `docs/screenshots/settings.png` | Settings page | Real — demo mode |

## Demo mode (no credentials required)

All screenshots except the calendar can be captured without any real email
account. Demo mode runs an in-memory mail client seeded with realistic messages.

```bash
# From the lilmail repo root — builds binary and captures all demo screenshots:
make demo-screenshots
```

This will:
1. Build the lilmail binary (`go build -o lilmail`)
2. Install Playwright Chromium if not already present (first run only)
3. Write a temporary `config.toml` with `[demo] enabled = true`
4. Start lilmail, log in as the demo user, capture all screenshots
5. Write PNGs to `docs/screenshots/` and stop the server

The demo seed contains:
- 10 INBOX messages (varied senders, subjects, dates; 3 unread; 2 with
  attachments; one 2-message thread; one 3-party thread)
- 2 Sent messages
- 1 Draft message

## Live IMAP mode (with real credentials)

To capture screenshots against a real account, set environment variables and
run `make screenshots`:

```bash
export LILMAIL_IMAP_SERVER=imap.example.com
export LILMAIL_IMAP_PORT=993
export LILMAIL_SMTP_SERVER=smtp.example.com
export LILMAIL_SMTP_PORT=587
export LILMAIL_USERNAME=you@example.com
export LILMAIL_PASSWORD=your-app-password
export LILMAIL_JWT_SECRET=$(openssl rand -hex 32)
export LILMAIL_ENC_KEY="a-32-character-encryption-key!!"

make screenshots
```

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Node.js | 18+ | Required to run the Playwright scripts |
| Go 1.23+ | — | To build the lilmail binary |

Playwright Chromium is installed automatically on first run.

## Reproducibility

The demo seed is hardcoded in `handlers/api/democlient.go`. Dates are relative
to `time.Now()` at server start, so timestamps differ between runs — but the
message content and folder structure are stable.

To reproduce exactly:

```bash
scripts/seed-demo.sh --screenshots
```

Or, to run the server and capture manually:

```bash
scripts/seed-demo.sh          # start; open browser at http://localhost:3099/demo-login
# Ctrl-C to stop
```

## docs/screenshots/README.md

See [`docs/screenshots/README.md`](screenshots/README.md) for per-file status.
