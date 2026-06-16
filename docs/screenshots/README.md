# Screenshots

This directory contains screenshots used in the README and documentation.

## Status

| File | Status | Notes |
|------|--------|-------|
| `hero.png` | Generated | Login page capture (no IMAP required) |
| `login.png` | Generated | Login page |
| `inbox.png` | Needs live IMAP | Requires a configured IMAP account |
| `message.png` | Needs live IMAP | Requires a message in the inbox |
| `compose.png` | Needs live IMAP | Compose modal triggered from inbox |
| `calendar.png` | Needs CalDAV | Requires `[caldav]` config + live CalDAV server |
| `settings.png` | Needs live IMAP | Settings page (session required) |
| `search.png` | Needs live IMAP | Search results page |

## Regenerating

From the repo root:

```bash
make screenshots
```

See [../SCREENSHOTS.md](../SCREENSHOTS.md) for full prerequisites and
environment variable configuration.
