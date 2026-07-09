# lilmail JSON API (`/v1`)

lilmail serves a clean JSON/REST API under `/v1`, **alongside** the
server-rendered HTMX UI. Both surfaces share the same mail engine and the same
session authentication — the JSON API is purely additive, so the standalone
HTMX experience is unchanged whether or not any client uses `/v1`.

It exists so rich clients can talk to lilmail over a stable, machine-readable
contract instead of scraping HTML fragments:

- **Vulos Mail** — the React webmail (`@vulos/mail-ui`).
- **Vulos Workspace** — the Workspace hub app's Mail surface.
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

### CP-brokered credential mode (Vulos Cloud)

In the Vulos Cloud deployment, lilmail runs **behind** the control plane (CP),
which custodies each user's **external** mailbox credentials (Gmail / Outlook /
generic IMAP) and reverse-proxies to `/v1`, injecting the per-request connection
credentials as HTTP headers. In this mode lilmail has no session of its own — the
mailbox identity and secret arrive on every request, and lilmail builds the IMAP/
SMTP client **directly from the headers** instead of from a session.

This path is **off by default** and is gated by a shared secret:

- Set `LILMAIL_BROKER_SECRET` (environment variable) on the lilmail process.
- The CP must send `X-Vulos-Broker-Auth: <secret>` on every brokered request.
  lilmail compares it against `LILMAIL_BROKER_SECRET` in **constant time**.
- **If `LILMAIL_BROKER_SECRET` is unset, or the presented secret does not match,
  the `X-Vulos-Mail-*` headers are ignored entirely** and the request falls back
  to normal session auth. Standalone lilmail therefore never trusts arbitrary
  client-supplied connection headers.

When the secret validates, lilmail reads the connection spec from these headers:

| Header | Meaning |
|--------|---------|
| `X-Vulos-Mail-Provider`  | `gmail` \| `outlook` \| `imap` (informational) |
| `X-Vulos-Mail-Email`     | mailbox address (used as MAIL FROM / identity) |
| `X-Vulos-Mail-Username`  | IMAP/SMTP login username (defaults to the email) |
| `X-Vulos-Mail-Auth`      | `xoauth2` (access token) \| `plain` (password) |
| `X-Vulos-Mail-Secret`    | the XOAUTH2 access token, or the IMAP/SMTP password |
| `X-Vulos-Mail-Imap-Host` | IMAP host (required) |
| `X-Vulos-Mail-Imap-Port` | IMAP port (default `993`, implicit TLS) |
| `X-Vulos-Mail-Smtp-Host` | SMTP host (defaults to the IMAP host) |
| `X-Vulos-Mail-Smtp-Port` | SMTP port (default `587`; `465` ⇒ implicit TLS, else STARTTLS) |
| `X-Vulos-Mail-Caldav-Url`  | CalDAV base URL for the account (optional; enables `/v1/calendar/*`) |
| `X-Vulos-Mail-Carddav-Url` | CardDAV base URL for the account (optional; enables `/v1/contacts`) |

`xoauth2` builds the IMAP client via `NewClientOAuth(host, port, username, token,
"xoauth2")` and the SMTP client via `NewSMTPClientOAuth`; `plain` uses
`NewClient(host, port, username, password)` / `NewSMTPClient`. The brokered path
covers the mail routes — folders, messages, single message, search, flags,
delete, move, compose (`POST /v1/messages`) and drafts (`POST /v1/drafts`).

**Calendar & contacts (brokered).** When the CP also sends
`X-Vulos-Mail-Caldav-Url` / `X-Vulos-Mail-Carddav-Url`, the `/v1/calendar/*` and
`/v1/contacts` routes are served from those per-account DAV base URLs instead of
the session. Authentication reuses `X-Vulos-Mail-Auth: xoauth2` +
`X-Vulos-Mail-Secret`: the access token is presented to CalDAV/CardDAV as an HTTP
`Authorization: Bearer <token>` header (via `api.NewCalDAVClient(cfg, token)` in
oauth2 mode and `api.CardDAVContactsBearer`). If the relevant DAV URL header is
**absent** in a brokered request, the read routes return an empty result
(`{ "events": [] }` / `{ "busy": [] }` / `{ "contacts": [] }`) and the write
routes (create/delete event) return `501 Not Implemented`
(`{ "error": "calendar not available for this account" }`) — the session is never
touched. These routes are registered whenever CalDAV/CardDAV is enabled **or** the
broker path is active (`LILMAIL_BROKER_SECRET` set), so they exist in CP
deployments even when the local `[caldav]`/`[carddav]` blocks are off.

Note: Outlook/Microsoft 365 calendar & contacts (Microsoft Graph) are **not**
covered by this CalDAV/CardDAV path; only accounts that expose CalDAV/CardDAV
(e.g. Gmail, generic DAV) work in brokered calendar/contacts mode.

The headers are only ever read **inside** the `/v1` group, after the broker
secret has been validated — never on unauthenticated or HTMX paths.

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
| `DELETE` | `/v1/messages/:uid`            | `folder`, `hard`        | —              | `204` |
| `POST`   | `/v1/messages/:uid/move`       | `folder`                | `{ toFolder, folder? }` | `204` |
| `POST`   | `/v1/messages`                 | —                       | `{ to, cc?, bcc?, subject, text?, html?, inReplyTo? }` | `201 { sent: true }` |
| `POST`   | `/v1/drafts`                   | —                       | `{ to?, cc?, subject?, text?, html?, inReplyTo? }`     | `201 { saved: true }` |

`DELETE /v1/messages/:uid` MOVES the message to the Trash folder by default
(discovered via the `\Trash` special-use, with name fallbacks Trash / Deleted /
Deleted Items / Bin). Pass `?hard=true` (or `?hard=1`) to permanently expunge
instead. If the source folder already IS the Trash folder, or no Trash folder
can be located, the delete falls back to a permanent expunge.

`POST /v1/messages/:uid/move` moves a message between folders (e.g. archive).
The source folder comes from the `folder` query param (default `INBOX`); an
optional non-empty `folder` field in the body overrides it. `toFolder` is
required.

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

# Delete (moves to Trash by default)
curl -b cookies.txt -X DELETE 'http://localhost:3000/v1/messages/42?folder=INBOX'

# Permanently delete (expunge, skip Trash)
curl -b cookies.txt -X DELETE 'http://localhost:3000/v1/messages/42?folder=INBOX&hard=true'

# Move / archive a message
curl -b cookies.txt -X POST 'http://localhost:3000/v1/messages/42/move?folder=INBOX' \
  -H 'Content-Type: application/json' -d '{"toFolder":"Archive"}'

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
