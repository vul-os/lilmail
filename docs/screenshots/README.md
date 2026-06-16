# Screenshots

This directory contains screenshots used in the README and documentation.

## Status

| File | Status | Notes |
|------|--------|-------|
| `hero.png` | Real (demo mode) | Inbox view with seeded messages |
| `login.png` | Real (demo mode) | Login page — no IMAP required |
| `inbox.png` | Real (demo mode) | Inbox with 10 seeded messages, threads, unread badges |
| `message.png` | Real (demo mode) | Message viewer open (GitHub PR notification) |
| `compose.png` | Real (demo mode) | Compose modal: TO/CC/BCC fields, subject, toolbar |
| `search.png` | Real (demo mode) | Search results filtered to "roadmap" thread |
| `calendar.png` | Needs CalDAV | Requires `[caldav]` config + live CalDAV server |
| `settings.png` | Real (demo mode) | Settings page |

## Regenerating

From the repo root (no credentials or external servers needed):

```bash
make demo-screenshots
```

See [../SCREENSHOTS.md](../SCREENSHOTS.md) for full prerequisites and options.
