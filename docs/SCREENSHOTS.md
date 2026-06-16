# Screenshots

This document describes the screenshot gallery, how to regenerate screenshots,
and which screenshots require a live IMAP account.

## Gallery

| File | Description | Requires live IMAP? |
|------|-------------|---------------------|
| `docs/screenshots/hero.png` | Hero composite (login + inbox) | No (login captured automatically) |
| `docs/screenshots/login.png` | Login page | No |
| `docs/screenshots/inbox.png` | Inbox / email list | **Yes** |
| `docs/screenshots/message.png` | Message viewer | **Yes** |
| `docs/screenshots/compose.png` | Compose modal | **Yes** (modal is triggered from inbox) |
| `docs/screenshots/calendar.png` | Calendar month view | **Yes** (requires `[caldav]` + live CalDAV) |
| `docs/screenshots/settings.png` | Settings page | **Yes** |
| `docs/screenshots/search.png` | Search results | **Yes** |

Screenshots that require a live IMAP account cannot be generated in a CI
environment without credentials. The login screenshot can be captured from the
public login page with no credentials.

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Node.js | 18+ | Required to run the Playwright script |
| Go 1.23+ | — | To build the lilmail binary |

Playwright Chromium is installed automatically by `make screenshots` the first
time you run it.

## Quick start

```bash
# From the lilmail repo root:
make screenshots
```

This will:
1. Build the lilmail binary (`go build -o lilmail`)
2. Start lilmail with the minimal demo config (no IMAP required)
3. Install Playwright Chromium if not already present
4. Capture the login page (and any other static routes)
5. Write PNGs to `docs/screenshots/`
6. Stop the server

## Full run with a live IMAP account

To capture inbox/message/compose/settings screenshots, provide real credentials
via environment variables before running `make screenshots`:

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

The script reads these environment variables and writes a temporary `config.toml`
before starting the server.

## BASE_URL override

By default the screenshotter connects to `http://localhost:3099` (a port chosen
to avoid conflicting with a running dev server). Override with:

```bash
BASE_URL=http://localhost:4000 node scripts/screenshots.mjs
```

Or if lilmail is already running (skip the auto-start):

```bash
LILMAIL_EXTERNAL=1 BASE_URL=http://localhost:3000 node scripts/screenshots.mjs
```

## Manual run

```bash
# Install Playwright once:
cd scripts && npm install && npx playwright install chromium

# Run the screenshotter (lilmail must already be running):
LILMAIL_EXTERNAL=1 BASE_URL=http://localhost:3000 node scripts/screenshots.mjs
```

## What the script captures

1. **Login page** — always captured; no credentials needed.
2. **Inbox** — attempts login with provided credentials; skipped with a warning
   if no credentials are set.
3. **Message view** — opens the first message in the inbox; skipped if inbox is
   empty or no credentials.
4. **Compose modal** — clicks Compose in the inbox; skipped if not logged in.
5. **Calendar** — navigates to `/calendar`; skipped if `[caldav]` is not enabled.
6. **Settings** — navigates to `/settings`; skipped if not logged in.
7. **Search results** — submits a search query; skipped if not logged in.

## docs/screenshots/README.md

See [`docs/screenshots/README.md`](screenshots/README.md) for a summary of which
screenshots are checked in versus which need to be generated locally.
