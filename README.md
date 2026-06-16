<div align="center">

# lilmail

**A lightweight, database-free webmail client written in Go.**

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/tag/exolutionza/lilmail?label=release&sort=semver)](https://github.com/exolutionza/lilmail/releases)
[![CI](https://github.com/exolutionza/lilmail/actions/workflows/ci.yml/badge.svg)](https://github.com/exolutionza/lilmail/actions/workflows/ci.yml)

![lilmail](docs/screenshots/hero.png)

</div>

---

## Overview

lilmail is a single Go binary webmail client that connects to any IMAP/SMTP
mailbox. It runs server-rendered HTML (Go templates + HTMX + Alpine.js) with all
frontend assets embedded — no build step, no CDN, no database. The binary runs
comfortably on 64 MB of RAM and is fully self-contained: only `config.toml`
needs to be present alongside it.

Connect with a classic username/password or **OAuth2/OpenID Connect** (XOAUTH2
and OAUTHBEARER SASL, full PKCE flow, automatic token refresh). Optional
features — CalDAV calendar, CardDAV contacts, AI mail assistant, real-time
notifications, Web Push, and multi-account support — are all opt-in via config
keys and add zero overhead when disabled.

## Screenshots

| Login | Inbox | Message view |
|-------|-------|--------------|
| ![Login](docs/screenshots/login.png) | ![Inbox](docs/screenshots/inbox.png) | ![Message](docs/screenshots/message.png) |

| Compose | Calendar | Settings |
|---------|----------|----------|
| ![Compose](docs/screenshots/compose.png) | ![Calendar](docs/screenshots/calendar.png) | ![Settings](docs/screenshots/settings.png) |

See [docs/SCREENSHOTS.md](docs/SCREENSHOTS.md) for the full gallery and how to
regenerate screenshots.

## Features

- **Single binary, no database** — templates and vendored JS embedded via
  `embed.FS`; runs fully offline/air-gapped with only `config.toml`
- **IMAP** mailbox browsing and **SMTP** sending
- **OAuth2 / OpenID Connect** — authorization-code flow, PKCE, automatic
  refresh-token handling, XOAUTH2 and OAUTHBEARER SASL
- **Conversation threading** — JWZ algorithm (`References`/`In-Reply-To`/
  `Message-ID`) backed by an embedded bbolt store
- **Compose** — plain-text and HTML rich-text (contenteditable toolbar); file
  attachments (multipart/mixed MIME); drafts with 30-second auto-save + IMAP
  APPEND/restore
- **Recipient autocomplete** — recent-recipients bbolt store + optional CardDAV
  address-book query
- **Calendar (CalDAV)** — month/week views, event creation, iTIP RSVP from
  invite attachments (opt-in via `[caldav]`)
- **Real-time notifications** — IMAP IDLE watcher, SSE stream, Web Notifications
  API, native desktop toasts, VAPID Web Push (opt-in via `[notifications]`)
- **AI mail assistant** — compose, summarize, reply suggestions, action-item
  extraction, phishing detection via any OpenAI-compatible endpoint (opt-in via
  `[ai]`)
- **Multiple accounts** — add/switch IMAP accounts; unified inbox with
  concurrent fan-out and per-account error isolation (opt-in via `[accounts]`)
- **Security** — JWT sessions, AES-GCM encrypted credentials at rest, full CSP,
  `SameSite=Lax` cookies, sandboxed email iframe
- **Dark mode** — hand-written CSS, no CDN dependency
- Runs on Linux, macOS, and Windows

## Quick start

```bash
# Clone
git clone https://github.com/exolutionza/lilmail.git
cd lilmail

# Configure — copy the example and fill in your mail server details + secrets
cp config.toml.example config.toml   # then edit

# Run
go run main.go
```

Open **http://localhost:3000** in your browser.

Prefer a pre-built binary? Grab the latest archive from
[Releases](https://github.com/exolutionza/lilmail/releases) — only
`config.toml` needs to be present alongside it.

## Documentation

| Document | Description |
|----------|-------------|
| [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) | Installation, first-run, and basic configuration walkthrough |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Code layout, request lifecycle, and subsystem overview |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | Complete `config.toml` reference — every key, section, and default |
| [docs/SCREENSHOTS.md](docs/SCREENSHOTS.md) | Screenshot gallery and how to regenerate with `make screenshots` |
| [ROADMAP.md](ROADMAP.md) | Shipped features, planned work, and exploratory ideas |
| [CHANGELOG.md](CHANGELOG.md) | Per-release changelog (Keep a Changelog format) |

## Development

```bash
# Build
go build -o lilmail

# Test
go test ./...

# Vet
go vet ./...

# Run (requires config.toml)
go run main.go

# Cross-compile
GOOS=linux   GOARCH=amd64 go build -o lilmail-linux-amd64
GOOS=darwin  GOARCH=amd64 go build -o lilmail-darwin-amd64
GOOS=windows GOARCH=amd64 go build -o lilmail-windows-amd64.exe
```

### Regenerate screenshots

```bash
make screenshots
```

This boots the lilmail binary, runs the Playwright screenshotter, and writes
PNGs to `docs/screenshots/`. See [docs/SCREENSHOTS.md](docs/SCREENSHOTS.md) for
prerequisites (Node 18+, Playwright Chromium) and which screenshots require a
live IMAP account.

## Contributing

Contributions are welcome. Please open an issue to discuss substantial changes
before sending a pull request. Make sure the following pass before submitting:

```bash
go build ./... && go vet ./... && go test ./...
```

## License

Released under the **MIT License** — see [LICENSE](LICENSE).
