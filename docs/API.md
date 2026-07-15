# lilmail `/v1` API — the PIM contract

`/v1` is lilmail's stable, machine-readable **contract**: JSON mail + calendar +
contacts served over one HTTP surface. lilmail is a standalone PIM client — it
connects to the **user's own** IMAP/SMTP/CalDAV/CardDAV account and exposes what
it reads/writes there as `/v1`. It hosts no mail itself and depends on no central
server.

`/v1` is the shared source of truth other UIs build on:

- the **Vulos OS** thin Calendar/Contacts widgets and mail surface consume it;
- lilmail's own server-rendered HTMX/Alpine UI is a first-class consumer too;
- any third-party tool or script can drive it.

The API returns `models.Email` / `MailboxInfo` / `models.Calendar*` /
`models.Contact` JSON and never renders templates, so it is a stable seam an
external UI can build against. It runs **alongside** the HTMX UI and shares the
same engine + authentication — using `/v1` never changes the standalone UI.

> **Stability.** `/v1` is load-bearing: external UIs build on it. The mail,
> `/v1/calendar/*`, and `/v1/contacts` shapes are treated as a stable contract —
> changes are additive.

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

### Injected-credential mode (embedding hosts)

Normally lilmail holds its own session and connects to the user's mailbox itself.
As an **option**, an embedding host (or the test harness) may inject the
per-request connection descriptor as HTTP headers, so lilmail builds the IMAP/
SMTP/DAV client **directly from the headers** instead of from a session. These
headers only ever describe the **user's own** account (endpoint + a short-lived
OAuth token or password) that lilmail then talks to — lilmail hosts no mail and
depends on no central server.

This path is **off by default** and gated by a shared secret:

- Set `LILMAIL_BROKER_SECRET` (environment variable) on the lilmail process.
- Every request must send `X-Vulos-Broker-Auth: <secret>`. lilmail compares it
  against `LILMAIL_BROKER_SECRET` in **constant time**.
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
`NewClient(host, port, username, password)` / `NewSMTPClient`. This path covers
the mail routes — folders, messages, single message, search, flags, delete, move,
compose (`POST /v1/messages`) and drafts (`POST /v1/drafts`).

**Calendar & contacts.** When `X-Vulos-Mail-Caldav-Url` /
`X-Vulos-Mail-Carddav-Url` are present, the `/v1/calendar/*` and `/v1/contacts`
routes are served from those per-account DAV base URLs instead of the session.
Authentication reuses `X-Vulos-Mail-Auth: xoauth2` + `X-Vulos-Mail-Secret`: the
access token is presented to CalDAV/CardDAV as an HTTP `Authorization: Bearer
<token>` header. If the relevant DAV URL header is **absent**, the read routes
return an empty result (`{ "events": [] }` / `{ "busy": [] }` /
`{ "contacts": [] }`) and the write routes return `501 Not Implemented` — the
session is never touched. These routes are registered whenever CalDAV/CardDAV is
enabled in config **or** the injected-credential path is active
(`LILMAIL_BROKER_SECRET` set).

Note: Outlook/Microsoft 365 calendar & contacts (Microsoft Graph) are **not**
covered by this CalDAV/CardDAV path; only accounts that expose CalDAV/CardDAV
(e.g. Gmail, generic DAV) work here.

The headers are only ever read **inside** the `/v1` group, after the secret has
been validated — never on unauthenticated or HTMX paths.

## Conventions

- **Folders** travel as the `folder` query parameter (default `INBOX`). This
  avoids escaping the IMAP hierarchy delimiter — `?folder=INBOX/Archive` works
  verbatim.
- **UIDs** are numeric and appear as path segments.
- **Pagination** on `/v1/messages`: `limit` caps the page size and `offset` skips
  the newest N messages, so page *k* = `?limit=L&offset=k*L`. The response echoes
  the effective `limit`/`offset` and sets `nextOffset` to the next page's offset
  (`null` when the returned page was short, i.e. the end of the mailbox).
- Errors are always `{ "error": "<message>" }` with an appropriate status code
  (`400` bad request, `401` unauthenticated, `404` not found, `409` conflict,
  `429` rate-limited, `501` not implemented, `502` upstream mail-server failure).
- All payloads follow the `models.Email` / `MailboxInfo` shapes (see
  `models/email.go`, `handlers/api/client.go`).

## Endpoints

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/me`                       | —                       | —              | `{ email, username }` |
| `GET`    | `/v1/folders`                  | —                       | —              | `{ folders: MailboxInfo[] }` |
| `POST`   | `/v1/folders`                  | —                       | `{ name }`     | `201 { folder }` |
| `DELETE` | `/v1/folders`                  | `folder`                | `{ name }` (or `?folder=`) | `204` |
| `GET`    | `/v1/messages`                 | `folder`, `limit` (50), `offset` (0) | —  | `{ folder, limit, offset, nextOffset, messages: Email[] }` |
| `GET`    | `/v1/messages/:uid`            | `folder`                | —              | `Email` (incl. `attachments[]`) |
| `GET`    | `/v1/messages/:uid/attachments/:partId` | `folder`       | —              | attachment bytes (streamed) |
| `GET`    | `/v1/search`                   | `folder`, `q`, `limit` (100) | —         | `{ folder, query, messages: Email[] }` |
| `PATCH`  | `/v1/messages/:uid/flags`      | `folder`                | `{ flag, add }` or `{ flags[], add }` | `204` |
| `DELETE` | `/v1/messages/:uid`            | `folder`, `hard`        | —              | `204` |
| `POST`   | `/v1/messages/:uid/move`       | `folder`                | `{ toFolder, folder? }` | `204` |
| `POST`   | `/v1/messages/:uid/spam`       | `folder`                | —              | `{ folder }` (Junk/Spam target) |
| `POST`   | `/v1/messages/:uid/snooze`     | `folder`                | `{ until }`    | `200 { snoozed, autoReturn:false, until, folder, note }` |
| `DELETE` | `/v1/messages/:uid/snooze`     | `folder`                | —              | `204` |
| `POST`   | `/v1/messages`                 | —                       | `{ to, cc?, bcc?, subject, text?, html?, inReplyTo?, attachments?, sendAt? }`¹ | `201 { sent: true }`, or `202 { scheduled, id, sendAt }` when `sendAt` is set |
| `POST`   | `/v1/drafts`                   | —                       | `{ to?, cc?, subject?, text?, html?, inReplyTo?, attachments? }`¹     | `201 { saved: true }` |
| `POST`   | `/v1/attachments`              | —                       | multipart form, file field `file` | `201 { token, filename, size, contentType }` |
| `GET`    | `/v1/scheduled`                | —                       | —              | `{ scheduled: […] }` (pending send-later) |
| `DELETE` | `/v1/scheduled/:id`            | —                       | —              | `204` |
| `PATCH`  | `/v1/scheduled/:id`            | —                       | `{ sendAt?, subject?, text?, html?, to?, cc?, bcc? }` | updated record |

¹ Each entry of `attachments[]` is `{ token? , data? , filename?, contentType?, contentId?, inline? }`
— see [Attachments](#attachments) for the two-step upload flow and `cid:` inline images.

`POST /v1/folders` creates an IMAP mailbox (a "label" in the mail-ui); the name
may not contain the IMAP control characters `\r \n \t * % "` and may not collide
with a protected system folder (Inbox/Sent/Drafts/Spam/Trash/Archive/Snoozed/…),
which return `409`. `DELETE /v1/folders` deletes a user mailbox by `name` (body)
or `?folder=`; system folders are `403`.

`POST /v1/messages/:uid/spam` reports spam by moving the message to the
discovered Junk/Spam folder — there is no separate training-signal endpoint on
this backend, so the move IS the report (pair it with an undo toast like archive).

`POST /v1/messages/:uid/snooze` moves the message to the Snoozed folder and, in a
validates + echoes `until`. lilmail is a client and does not itself run a
delivery-side scheduler, so it does **not** auto-return the message to the inbox:
the response is `200 { snoozed:true, autoReturn:false, until, folder, note }` and
the client is responsible for surfacing the due time / returning the message.
`DELETE /v1/messages/:uid/snooze` is a no-op acknowledgement kept for symmetry
(the caller moves the message back itself); it returns `204`.

`DELETE /v1/messages/:uid` MOVES the message to the Trash folder by default
(discovered via the `\Trash` special-use, with name fallbacks Trash / Deleted /
Deleted Items / Bin). Pass `?hard=true` (or `?hard=1`) to permanently expunge
instead. If the source folder already IS the Trash folder, or no Trash folder
can be located, the delete falls back to a permanent expunge.

`POST /v1/messages/:uid/move` moves a message between folders (e.g. archive).
The source folder comes from the `folder` query param (default `INBOX`); an
optional non-empty `folder` field in the body overrides it. `toFolder` is
required.

`POST /v1/messages` is **rate-limited per client IP** to prevent spam/relay abuse
(default 30 sends / 60 s, configurable via `[rate_limit]` in `config.toml`); the
cap returns `429 { "error": "rate limit exceeded" }`.

### Attachments

**Download.** `GET /v1/messages/:uid/attachments/:partId?folder=` streams a
single MIME part on demand (the content is NOT included in the message listing).
`partId` is the IMAP MIME part path — take it from an entry's `partId` in the
message's `attachments[]` array (see the `Email` shape below). The response
carries the part's `Content-Type` and a `Content-Disposition: attachment;
filename="…"` (with an RFC 5987 `filename*` form for non-ASCII names). Both the
content type and filename are sanitized against response-header injection; an
untrusted/malformed content type falls back to `application/octet-stream`.
Downloads are capped at 25 MiB. Works in both session and injected-credential modes.
Unknown part / message ⇒ `404`; unauthenticated ⇒ `401`.

**Upload (compose).** Attaching a file to an outgoing message is a two-step,
JSON-friendly flow:

1. `POST /v1/attachments` — multipart form with a `file` field. Stages the bytes
   under the caller's per-account namespace and returns
   `201 { token, filename, size, contentType }`.
2. `POST /v1/messages` (or `/v1/drafts`) — reference the staged upload in the
   `attachments` array: `{"attachments":[{"token":"<token>"}]}`. Each token is
   resolved and CONSUMED (single-use, so it cannot be replayed). `filename` /
   `contentType` may be supplied per-entry to override the staged metadata.

Alternatively, small files can be sent fully inline (no step 1) with
`{"filename":"a.txt","contentType":"text/plain","data":"<base64>"}`. A single
attachment (and the per-message total) is capped at 25 MiB. Staged uploads that
are never sent are garbage-collected after 24 h.

**Inline images (`cid:`).** To embed an image *inside* the HTML body (rather than
send it as a downloadable file), mark the attachment ref `inline` and give it a
`contentId`, then reference that id from the HTML with `<img src="cid:ID">`:

```json
{
  "to": "bob@example.com",
  "subject": "Hi",
  "html": "<p>See <img src=\"cid:logo1\"></p>",
  "attachments": [
    { "contentId": "logo1", "inline": true,
      "contentType": "image/png", "data": "<base64>" }
  ]
}
```

The two new per-entry fields extend the existing attachment ref shape:

| field       | type    | meaning |
|-------------|---------|---------|
| `contentId` | string  | Bare cid token — **no** `cid:` scheme, **no** angle brackets. Must match the `cid:ID` in the HTML. Required when `inline`. Validated against header injection (`[A-Za-z0-9._%+-]+(@host)?`; CRLF/space/`<>"` rejected). |
| `inline`    | boolean | `true` ⇒ the part is emitted with `Content-Disposition: inline` and a `Content-ID: <contentId>` header inside a `multipart/related` container, so it renders in-body. |

`inline` and the byte source are orthogonal: an inline image can be supplied by
`token` (staged upload) **or** by base64 `data`. An `inline` ref without a
`contentId` is a `400`; an `inline` ref on a message with no `html` body degrades
to a normal attachment (nothing could reference it).

Resulting MIME structure:

- **inline only** → `multipart/related( multipart/alternative(text, html), inline-parts… )`
- **inline + regular attachments** →
  `multipart/mixed( multipart/related( multipart/alternative(text, html), inline-parts… ), attachments… )`
- **no inline parts** → unchanged (`multipart/mixed`/`alternative`/plain as before).

This lets a client stop shipping fat `data:image/…;base64,…` URIs inside the HTML
body (which inflate every message ~33 %) and reference `cid:` parts instead. The
client-side switch (paste handler emitting `cid:` + an inline attachment ref) is
a follow-up; the backend is capable and documented as of wave 44.

### Scheduled send (send-later)

`POST /v1/messages` with a **future** RFC3339 `sendAt` turns an ordinary send into
a scheduled one: the compose payload is persisted and delivered at the due time by
a background drain, and the call returns `202 { scheduled:true, id, sendAt }`
instead of `201 { sent:true }`. Omit `sendAt` (or pass an empty string) for an
immediate send. A past/absurd `sendAt` is `400`; a value beyond one year is `400`.
For accounts authenticated with a short-lived OAuth token (injected Gmail/Outlook creds,
or a session OAuth login) the horizon tightens to ~12 h — beyond that the captured
token would be expired at fire time, so the schedule is refused up front rather
than accepted and silently failed.

| Method | Path | Body | Returns |
|--------|------|------|---------|
| `GET`    | `/v1/scheduled`      | —                                          | `{ scheduled: [{ id, sendAt, to, cc?, bcc?, subject, created }] }` |
| `DELETE` | `/v1/scheduled/:id`  | —                                          | `204` |
| `PATCH`  | `/v1/scheduled/:id`  | `{ sendAt?, subject?, text?, html?, to?, cc?, bcc? }` (all optional) | updated public record |

Scheduled sends are scoped to the authenticated account: another account's `id`
(or a nonexistent one) is `404`, so listing/cancel/edit never leak across
accounts. Delivery is **at-least-once** — the record is deleted only after a
successful SMTP send, so a crash mid-send re-fires it on the next poll (a rare
duplicate) rather than dropping the mail; a persistently failing send is abandoned
after a bounded retry budget. Every fire rebuilds the MIME through the same
`BuildMIMEMessage` engine as an immediate send, so the header-injection guard and
`cid:` inline handling run at actual send time.

Scheduled send is **enabled by wiring a durable KV store** into the API handler
(`NewWithStore`). Where it is not configured, `POST /v1/messages` with `sendAt`
and the whole `/v1/scheduled` surface return `501 Not Implemented` — an
unconfigured build simply has no send-later, rather than silently dropping mail.

### Calendar (only when `[caldav] enabled`)

Times are RFC 3339 strings. The `start`/`end` range defaults to the current
month when omitted. These reuse the same CalDAV client + `models.Calendar*`
types as the HTMX calendar UI.

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/calendar/events`          | `start`, `end` | —          | `{ events: CalendarEvent[] }` |
| `POST`   | `/v1/calendar/events`          | —              | `{ summary, start, end, description?, location?, allDay?, recurrence? }` | `201 { created: true }` |
| `PUT`    | `/v1/calendar/events/:uid`     | —              | `{ summary, start, end, description?, location?, allDay?, recurrence?, path? }` | `{ updated: true }` |
| `DELETE` | `/v1/calendar/events/:uid`     | —              | —          | `204` |
| `GET`    | `/v1/calendar/freebusy`        | `start`, `end` | —          | `{ busy: { start, end }[] }` |
| `POST`   | `/v1/calendar/rsvp`            | —              | `{ uid, organizer, response, …event }` | `{ ok: true }` |

`recurrence` is a raw iCalendar RRULE (e.g. `FREQ=WEEKLY;COUNT=10`), stored and
returned verbatim. `CalendarEvent` includes `uid`, `path` (CalDAV object path)
and `recurrence`; pass `path` back on `PUT` so an edit targets the exact object.

`POST /v1/calendar/rsvp` is the **iTIP/iMIP** reply to a received invitation: it
sends a `METHOD:REPLY` back to the `organizer` with the chosen `response`
(`ACCEPTED` / `DECLINED` / `TENTATIVE`) and reflects the event into the
responder's own calendar. A received invite arrives on a message as
`Email.Invite` (attendees plus the recipient's own `MyPartStat`), which the client
reads from `GET /v1/messages/:uid`.

### Contacts (only when `[carddav] enabled`)

| Method | Path | Query | Body | Returns |
|--------|------|-------|------|---------|
| `GET`    | `/v1/contacts`                 | `q`, `limit` (50)  | —       | `{ contacts: { email, name }[] }` (autocomplete form) |
| `GET`    | `/v1/contacts/cards`           | `q`, `limit` (500) | —       | `{ contacts: Contact[] }` (full cards) |
| `POST`   | `/v1/contacts`                 | —                  | `{ name, emails, phones?, org?, title?, note? }` | `201 { contact }` |
| `PUT`    | `/v1/contacts/:uid`            | —                  | `{ name, emails, phones?, org?, title?, note?, path? }` | `{ contact }` |
| `DELETE` | `/v1/contacts/:uid`            | `path?`            | —       | `204` |

`Contact` = `{ uid, name, org?, title?, note?, emails[], phones?, path? }`. The
`path` is the CardDAV object path; pass it back on `PUT`/`DELETE` to target the
exact card.

### Settings — vacation responder (`/v1/settings/vacation`)

Per-account out-of-office responder config. Durable-KV backed — returns `501`
when no store is wired. Owner is the authenticated identity (session email or
injected-credential mailbox); a caller only ever reads/writes **their own** config.

| Method | Path | Body | Returns |
|--------|------|------|---------|
| `GET` | `/v1/settings/vacation` | — | `VacationConfig` |
| `PUT` | `/v1/settings/vacation` | `VacationConfig` | `VacationConfig` |

```jsonc
// VacationConfig
{
  "enabled": true,
  "subject": "Out of office",       // becomes a real mail Subject → CR/LF/NUL rejected (400)
  "body": "<p>Back Monday</p>",     // HTML, sanitized server-side (script/handlers/js: stripped)
  "startAt": "2026-07-10T00:00:00Z", // optional RFC3339; responder inactive before this
  "endAt":   "2026-07-20T00:00:00Z", // optional RFC3339; inactive after this; must be ≥ startAt
  "respondOnlyToContacts": false     // limit auto-replies to known contacts (anti-backscatter)
}
```

An enabled responder requires a non-empty `subject`. The body is sanitized (a
stored-XSS payload cannot ride the auto-reply). Loop/backscatter protection is
built in: auto-replies are never sent to another auto-reply (`Auto-Submitted`),
to list mail (`List-*` / `Precedence: bulk`), or to a null/bounce sender.

**Enforcement note:** lilmail is a **client** — it connects to the user's provider
over IMAP/SMTP and does **not** run the inbound delivery path, so storing
`enabled:true` here does not by itself make the provider auto-reply. This endpoint
is the authoritative **config** the client edits and stores; actual enforcement
must be set on the provider's own vacation/out-of-office feature (Gmail, Fastmail,
a self-hosted Dovecot/Sieve, …). The GET always echoes the stored config so the UI
stays truthful about what it holds.

### Settings — signatures (`/v1/settings/signatures`)

Multiple named HTML signatures. `PUT` replaces the whole set. Each signature's
HTML is sanitized. The server assigns an `id` when omitted (create by omitting).
At most one signature may be `default:true`.

| Method | Path | Body | Returns |
|--------|------|------|---------|
| `GET` | `/v1/settings/signatures` | — | `{ signatures: Signature[] }` |
| `PUT` | `/v1/settings/signatures` | `{ signatures: Signature[] }` | `{ signatures: Signature[] }` |

```jsonc
// Signature
{ "id": "a1b2c3d4", "name": "Work", "html": "<b>Jane Doe</b>", "default": true }
```

### Settings — send-as identities (`/v1/settings/identities`)

The From/identity list the compose window offers. The primary mailbox is **always**
returned first with `isPrimary:true` (never removable), followed by any send-as
aliases. Each identity may link a default signature by id.

| Method | Path | Body | Returns |
|--------|------|------|---------|
| `GET` | `/v1/settings/identities` | — | `{ identities: Identity[] }` |
| `PUT` | `/v1/settings/identities` | `{ identities: Identity[] }` | `{ identities: Identity[] }` |

`PUT` **replaces the whole set of aliases**. The primary mailbox is implicit: it is
never stored, never writable, and always re-added on read.

**lilmail is a client, not the send-as authority.** These identities are the
**client's read model** — the list the compose window offers. The user's own
provider SMTP server remains the authority for what From it will actually accept,
and re-checks it at submission time. Each address is still validated locally for
shape and header-injection (CR/LF/NUL in the address or name ⇒ `400`) before it is
stored. Nothing here makes an address inbound-deliverable — inbound delivery is the
provider's concern, not lilmail's.

Compose honours the choice: `POST /v1/messages` (and `/v1/drafts`) accept
`"from": "<address>"`, which must be the primary mailbox or a **registered**
identity — anything else is `403` (and the provider's SMTP server re-checks it at
submission). A scheduled send fires with that From but the record stays **owned**
by the authenticated mailbox.

```jsonc
// Identity
{ "address": "me@example.com", "name": "Me", "isPrimary": true, "defaultSignatureId": "a1b2c3d4" }
```

### Connected accounts + unified inbox (`/v1/accounts`, `/v1/unified`)

Additional mailboxes the user has connected, and a merged read across all of them.
Durable-KV backed (`501` without a store). Credentials are **AES-GCM encrypted at
rest** with the app key and are **never** returned in any response. Strict
per-user isolation: a user lists/adds/removes/reads only their own accounts;
another user's account is `404` (no-leak).

| Method | Path | Body | Returns |
|--------|------|------|---------|
| `GET`    | `/v1/accounts`         | — | `{ accounts: ConnectedAccount[] }` (no secrets) |
| `POST`   | `/v1/accounts`         | `AddAccount` | `201 ConnectedAccount` |
| `DELETE` | `/v1/accounts/:email`  | — | `204` (own) / `404` (foreign or missing) |
| `GET`    | `/v1/unified`          | `?folder=&limit=` | `{ folder, messages: Email[], errors: [] }` |
| `GET`    | `/v1/messages?account=all` | `?folder=&limit=` | same as `/v1/unified` (alias) |

```jsonc
// AddAccount (POST body) — password validated against the live IMAP server first
{ "email":"work@corp.com", "password":"…", "label":"Work", "color":"#0a0",
  "imapServer":"imap.corp.com", "imapPort":993, "smtpServer":"smtp.corp.com", "smtpPort":587 }

// ConnectedAccount (response) — password fields OMITTED, never serialized
{ "email":"work@corp.com", "label":"Work", "color":"#0a0",
  "imapServer":"imap.corp.com", "imapPort":993, "smtpServer":"smtp.corp.com", "smtpPort":587 }
```

The unified fetch runs one connection per account concurrently (each with its own
timeout); **one failing account never breaks the others** — its failure appears in
the `errors` array (`{account, error}`) alongside the messages that did load. Each
merged message is tagged with its source via `accountEmail` / `accountLabel` /
`accountColor` on the `Email` shape, and the list is newest-first, capped at 200.

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

# Set the vacation responder
curl -b cookies.txt -X PUT http://localhost:3000/v1/settings/vacation \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"subject":"Out of office","body":"<p>Back Monday</p>"}'

# Add a connected account (password is validated against IMAP, then encrypted at rest)
curl -b cookies.txt -X POST http://localhost:3000/v1/accounts \
  -H 'Content-Type: application/json' \
  -d '{"email":"work@corp.com","password":"…","label":"Work","imapServer":"imap.corp.com"}'

# Unified inbox across the primary + all connected accounts
curl -b cookies.txt 'http://localhost:3000/v1/unified?folder=INBOX&limit=50'
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
  "attachments": [
    {
      "id": "SU5CT1gAMzQAMi4x",   // opaque token (HTMX web download route)
      "partId": "2.1",             // IMAP MIME part path — use with the /v1 download route
      "filename": "invoice.pdf",
      "contentType": "application/pdf",
      "size": 84320,
      "isInline": false
    }
  ],
  "flags": ["\\Seen"],
  "messageId": "<…@example.com>",
  "inReplyTo": "<…>",
  "references": ["<…>"],
  "accountEmail": "work@corp.com",   // only in unified results — source account tag
  "accountLabel": "Work",
  "accountColor": "#0a0",
  "auth": {                           // only on a single-message read; omitted if no header
    "spf": "pass",                    // pass|fail|softfail|neutral|none|temperror|permerror
    "dkim": "pass",
    "dmarc": "pass",
    "dkimDomain": "sender.com",
    "raw": "mx.example.com; spf=pass …" // verbatim Authentication-Results value
  }
}
```

`attachments[]` is metadata only (no bytes). Build a download link as
`/v1/messages/{id}/attachments/{partId}?folder={folder}`. `isInline` flags parts
meant to render inline (e.g. embedded images) versus regular file attachments.

`auth` (present on a single-message read, `GET /v1/messages/:uid`) surfaces the
**receiving server's** SPF/DKIM/DMARC verdict, parsed read-only from the message's
`Authentication-Results` header (RFC 8601). lilmail does not re-verify; it exposes
the trusted receiver's stamp so the client can render a "verified sender" / "why in
spam" badge. `null`/absent when the message carries no such header.

## Demo mode

When `[demo] enabled = true`, the API is backed by the in-memory `DemoClient`
(no IMAP contact), so it returns seeded data — useful for building/screenshotting
clients without a live mailbox.

## Not yet exposed

Recurring-event expansion (server-side RRULE materialization) is not yet exposed
over `/v1`. Track this in [ROADMAP.md](../ROADMAP.md).
