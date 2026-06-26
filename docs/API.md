# lilmail JSON API (`/v1`)

lilmail serves a clean JSON/REST API under `/v1`, **alongside** the
server-rendered HTMX UI. Both surfaces share the same mail engine and the same
session authentication — the JSON API is purely additive, so the standalone
HTMX experience is unchanged whether or not any client uses `/v1`.

It exists so rich clients can talk to lilmail over a stable, machine-readable
contract instead of scraping HTML fragments:

- **Vulos Mail** — the React webmail (`@vulos/mail-ui`).
- **Vulos Workspace** — the suite shell's Mail surface.
- Any third-party tool or script.

The HTMX UI keeps rendering server-side HTML; the JSON API returns
`models.Email` / `MailboxInfo` JSON and never renders templates.

## Authentication

The API reuses lilmail's session cookie — the **same** session established by
`POST /login` or the OAuth2 flow. There is no separate API token scheme.

The one behavioural difference from the HTMX routes: when the session is missing
or unauthenticated, the API responds **`401` with a JSON body** (the HTMX UI
`302`-redirects to `/login`), so a `fetch()`-based client can react in code:

```json
{ "error": "not authenticated" }
```

Send requests with credentials included (e.g. `fetch(url, { credentials: 'include' })`).

## Conventions

- **Folders** travel as the `folder` query parameter (default `INBOX`). This
  avoids escaping the IMAP hierarchy delimiter — `?folder=INBOX/Archive` works
  verbatim.
- **UIDs** are numeric and appear as path segments.
- Errors are always `{ "error": "<message>" }` with an appropriate status code
  (`400` bad request, `401` unauthenticated, `404` not found, `502` upstream
  mail-server failure).
- All payloads follow the `models.Email` / `MailboxInfo` shapes (see
  `models/email.go`, `handlers/api/client.go`).

## Endpoints

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/me`                       | —                       | —              | `{ email, username }` |
| `GET`    | `/v1/folders`                  | —                       | —              | `{ folders: MailboxInfo[] }` |
| `GET`    | `/v1/messages`                 | `folder`, `limit` (50)  | —              | `{ folder, messages: Email[] }` |
| `GET`    | `/v1/messages/:uid`            | `folder`                | —              | `Email` |
| `GET`    | `/v1/search`                   | `folder`, `q`, `limit` (100) | —         | `{ folder, query, messages: Email[] }` |
| `PATCH`  | `/v1/messages/:uid/flags`      | `folder`                | `{ flag, add }`| `204` |
| `DELETE` | `/v1/messages/:uid`            | `folder`                | —              | `204` |
| `POST`   | `/v1/messages`                 | —                       | `{ to, cc?, bcc?, subject, text?, html?, inReplyTo? }` | `201 { sent: true }` |
| `POST`   | `/v1/drafts`                   | —                       | `{ to?, cc?, subject?, text?, html?, inReplyTo? }`     | `201 { saved: true }` |

### Calendar (only when `[caldav] enabled`)

Times are RFC 3339 strings. The `start`/`end` range defaults to the current
month when omitted. These reuse the same CalDAV client + `models.Calendar*`
types as the HTMX calendar UI.

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/calendar/events`          | `start`, `end` | —          | `{ events: CalendarEvent[] }` |
| `POST`   | `/v1/calendar/events`          | —              | `{ summary, start, end, description?, location?, allDay? }` | `201 { created: true }` |
| `DELETE` | `/v1/calendar/events/:uid`     | —              | —          | `204` |
| `GET`    | `/v1/calendar/freebusy`        | `start`, `end` | —          | `{ busy: { start, end }[] }` |

### Contacts (only when `[carddav] enabled`)

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/contacts`                 | `q`, `limit` (50) | —       | `{ contacts: { email, name }[] }` |

### Examples

```bash
# List mailboxes
curl -b cookies.txt http://localhost:3000/v1/folders

# 50 most recent messages in INBOX
curl -b cookies.txt 'http://localhost:3000/v1/messages?folder=INBOX&limit=50'

# Read one message
curl -b cookies.txt 'http://localhost:3000/v1/messages/42?folder=INBOX'

# Full-text search
curl -b cookies.txt 'http://localhost:3000/v1/search?folder=INBOX&q=invoice'

# Mark as read (\Seen)  /  star (\Flagged)
curl -b cookies.txt -X PATCH 'http://localhost:3000/v1/messages/42/flags?folder=INBOX' \
  -H 'Content-Type: application/json' -d '{"flag":"\\Seen","add":true}'

# Delete
curl -b cookies.txt -X DELETE 'http://localhost:3000/v1/messages/42?folder=INBOX'

# Send a message
curl -b cookies.txt -X POST http://localhost:3000/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"to":"alice@example.com","subject":"Hi","text":"Hello from /v1"}'

# Save a draft
curl -b cookies.txt -X POST http://localhost:3000/v1/drafts \
  -H 'Content-Type: application/json' \
  -d '{"to":"alice@example.com","subject":"WIP","text":"unfinished…"}'

# List calendar events for a range (CalDAV must be enabled)
curl -b cookies.txt 'http://localhost:3000/v1/calendar/events?start=2026-06-01T00:00:00Z&end=2026-07-01T00:00:00Z'

# Search contacts (CardDAV must be enabled)
curl -b cookies.txt 'http://localhost:3000/v1/contacts?q=alice'
```

### `Email` shape (abridged)

```jsonc
{
  "id": "42",
  "from": "alice@example.com",
  "fromName": "Alice",
  "to": "me@example.com",
  "subject": "Invoice",
  "preview": "Here is the invoice you asked for…",
  "body": "plain text body",
  "html": "<p>…</p>",
  "date": "2026-06-26T10:00:00Z",
  "hasAttachments": true,
  "flags": ["\\Seen"],
  "messageId": "<…@example.com>",
  "inReplyTo": "<…>",
  "references": ["<…>"]
}
```

## Demo mode

When `[demo] enabled = true`, the API is backed by the in-memory `DemoClient`
(no IMAP contact), so it returns seeded data — useful for building/screenshotting
clients without a live mailbox.

## Not yet exposed

Attachment upload/download over `/v1` is still HTMX-only (compose accepts text +
HTML bodies via JSON; multipart attachment handling remains on the web routes).
Calendar event editing (`PUT`) and recurring-event expansion are not yet exposed.
Track these in [ROADMAP.md](../ROADMAP.md).
