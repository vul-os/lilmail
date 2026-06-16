# LilMail Roadmap 🗺️

LilMail aims to be a **simple, self-hostable, resource-light alternative to
Thunderbird** for the web — a single Go binary, no database to administer, and
a UI that stays out of your way. This document tracks where we are and where
we're going.

> Scope philosophy: stay small and fast. Every feature below is weighed against
> the "runs on 64 MB of RAM, no external services" promise. Power-user features
> are welcome as long as they don't compromise that.
>
> 🎨 **Look & feel:** Gmail-inspired layout and density (sticky top bar,
> collapsible sidebar, docked compose, calendar), hand-written CSS with dark
> mode. Target: feature parity with **Thunderbird**.
>
> 📋 The concrete, checkbox-level breakdown lives in **[TASKS.md](TASKS.md)**.

## Status legend

| Symbol | Meaning |
| ------ | ------- |
| ✅ | Shipped |
| 🚧 | In progress |
| 🔜 | Planned (near-term) |
| 💭 | Exploratory / longer-term |

---

## ✅ Shipped

**Core mail**
- IMAP folder browsing and message viewing; SMTP sending + save-to-Sent
- File-based caching, no database required
- JWT sessions + AES-GCM encrypted credentials/tokens at rest
- Full-email-as-username support (`username_is_email`)

**Authentication**
- **OAuth2 / OpenID Connect** with **XOAUTH2** and **OAUTHBEARER** SASL for
  both IMAP and SMTP — authorization-code flow, **PKCE**, automatic
  refresh-token handling — alongside classic password login

**Attachments**
- Metadata read from `BODYSTRUCTURE`; content fetched on demand per MIME part
  (base64 / quoted-printable decoded) and streamed via `GET /api/attachment/:id`
  — the inbox list never buffers attachment bytes

**Scale & security**
- Path-safe cache (username sanitized → no path traversal); `0700` dirs /
  `0600` files; atomic cache writes
- SMTP TLS certificate verification (opt-out only via `insecure_skip_verify`)
- Request `BodyLimit` + attachment size guard
- Full **Content-Security-Policy** (script-src, frame-ancestors) + `SameSite=Lax`
  session cookie; email HTML rendered in a sandboxed `<iframe>` with no scripts
  (srcdoc XSS closed)

**UI/UX — Gmail-inspired, responsive, dark mode**
- Responsive app shell: sticky top bar + collapsible sidebar / mobile drawer
- Gmail-like message list (density, unread emphasis, hover actions)
- Docked compose (bottom-right; full-screen on mobile)
- Message viewer renders HTML mail in a **sandboxed iframe** (no scripts / no
  same-origin) with plain-text fallback
- Hand-written CSS design system (dark mode, no CDN dependency)

**AI mail assistant** (opt-in via `[ai].enabled`)
- Compose / summarize / reply suggestions / action-item extraction / phishing
  detection — delegates to a configurable OpenAI-compatible SSE endpoint
- Default endpoint targets the Vulos OS airouter; fully standalone with any
  provider

**Conversation threading**
- **JWZ algorithm** (`References` / `In-Reply-To` / `Message-ID`) grouping
  messages into conversations, backed by an embedded **bbolt** store
- Collapse/expand conversation UI with message counts

**Calendar (CalDAV)**
- CalDAV client (discover calendars, list events, create events) via
  `emersion/go-webdav` + `emersion/go-ical`
- Gmail-style month & week views; iCalendar invite detection in the mail viewer
  with a basic RSVP affordance — all opt-in via `[caldav].enabled`

**Notifications & real-time** (opt-in via `[notifications].enabled`)
- Server-side **IMAP IDLE** watcher
- **SSE** stream → **Web Notifications API** (OS notifications while a tab is open)
- Opt-in **native desktop** toasts via `gen2brain/beeep` for local runs
- **Web Push / VAPID** — background push delivery (even with no open tab); auto-generated
  ECDH P-256 VAPID key pair; `/api/push/vapid-public`, `POST/DELETE /api/push/subscribe`;
  service worker (`/sw.js`) handles `push` + `notificationclick` + `pushsubscriptionchange`
  events; expired subscriptions (HTTP 410) auto-removed; Settings page toggle

**Vulos OS embed**
- `[server] frame_ancestors` config injects into `Content-Security-Policy:
  frame-ancestors` so the Vulos shell can embed LilMail as an iframe

**Packaging & distribution**
- **Self-contained binary** — templates and vendored HTMX/Alpine.js are
  embedded via `embed.FS`; runs fully offline with only `config.toml`
- Unit tests (SASL/MIME/attachment-ID/threading/AI/config) + **CI** workflow
- Semantic-versioned **release pipeline** (GitHub Actions on `v*` tags)

---

## ✅ Recently shipped (v1.6.0)

- **Mark-as-unread** — real `IMAP UID STORE -FLAGS \Seen` via new route
  `PATCH /api/email/:id/unread`; list refreshes automatically.
- **Search** — `IMAP UID SEARCH TEXT` via `GET /api/search?q=…`; top-bar
  search box wired with hx-get, 500 ms debounce.
- **Reply/Forward threading** — `In-Reply-To` and `References` headers wired
  from the reply button through the SMTP send path.
- **CC/BCC** — collapsible CC/BCC fields in compose modal; full envelope support.
- **SMTP implicit TLS (port 465)** — `use_starttls = false` now uses `tls.Dial`.
- **Sent-folder discovery** — `\Sent` special-use attribute via `IMAP LIST`.
- **Real iTIP RSVP** — `METHOD:REPLY` iCalendar sent to organiser via SMTP.
- **`[server] secure_cookies`** config key for TLS-terminated deployments.
- **Shared bbolt handle** per user (no per-request file open).

## ✅ Recently shipped (v1.7.0)

- **Drafts** — Save / list / restore via IMAP APPEND to the `\Drafts` folder;
  compose modal "Draft" button + 30-second auto-save; draft restored into
  compose on click; draft deleted from IMAP on send.
- **Attachments in compose** — multipart form upload; `multipart/mixed` MIME
  assembly (base64 attachment parts, correct `Content-Type`/`Content-Disposition`);
  filename list shown in compose before send.
- **HTML compose** — lightweight contenteditable rich-text editor with
  dependency-free formatting toolbar (bold/italic/underline/lists/links);
  sends `multipart/alternative` (plain + HTML); plain compose unchanged.
- **Recipient autocomplete** — inline dropdown on To/CC/BCC; backed by
  recent-recipients bbolt store (records every send) + optional CardDAV
  address-book query; `GET /api/autocomplete?q=` endpoint.
- **CardDAV contacts** (`[carddav]` config) — optional; uses `go-vcard`
  `FN`/`EMAIL` fields from configured address book.

## ✅ Recently shipped (v1.9.0)

- **Unified inbox** — when `[accounts] enabled = true` and additional accounts
  are configured, a "Unified" toggle appears in the inbox toolbar. Clicking it
  fans out to all accounts concurrently (one goroutine per account, 10 s timeout
  each) and merges results newest-first. Each row shows the source account as a
  colour badge. One failing account does not suppress the rest — a per-account
  error indicator (red dot) appears in the toolbar instead. Clicking a message
  in unified mode fetches it from the correct account's IMAP connection
  (`X-Account-Email` header); replying or sending uses that account's SMTP
  credentials (`account_email` form field wired through compose). Degrades
  gracefully: when only one account exists the toggle is hidden and inbox
  behaviour is unchanged.

## ✅ Recently shipped (v1.8.0)

- **Web Push (VAPID + Service Worker)** — background push notifications when no
  tab is open. Auto-generated VAPID key pair; push subscription management
  (`/api/push/subscribe`); service worker `/sw.js` handles `push`,
  `notificationclick`, and `pushsubscriptionchange`; Settings page toggle.
  Opt-in via `[notifications] webpush = true`; degrades gracefully when unconfigured.
- **Multiple accounts / account switcher** — add extra IMAP/SMTP accounts from
  the Settings page; credentials validated on add, stored AES-256-GCM encrypted
  in `accounts.db`; switch active session without logging out; the previous
  identity is saved as an additional account so switching back works immediately.
  Opt-in via `[accounts] enabled = true`.
- **Settings page** (`GET /settings`) — always registered; shows push
  notification toggle when `webpush = true`, account management when
  `accounts.enabled = true`; linked from a gear icon in the top bar.

## 🔜 Next up

- 🔜 **Nix package + NixOS module** for declarative, reproducible self-hosting.

## 💭 Later / exploratory

- 💭 **Filters / rules** and richer server-side flag management.
- 💭 **PWA / offline** mode and a keyboard-driven UX / theming.
- 💭 **JMAP** client support ([RFC 8620](https://jmap.io/)) as a modern transport
  alongside IMAP/SMTP — abstract the mail backend behind an interface.
- 💭 **OpenPGP** and **S/MIME** sign / encrypt / verify.
- 💭 Container image and Helm chart.

---

## Non-goals

- Becoming a heavyweight groupware suite.
- Requiring an external database or message broker.
- Bundling a full HTML rendering engine / tracking-pixel-friendly mail viewer.

Have an idea or want to pick something up? See
[Contributing](README.md#-contributing) and open an issue or PR.
