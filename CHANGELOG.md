# Changelog

All notable changes to LilMail are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)  
Versioning: [Semantic Versioning](https://semver.org/)

---

## [Unreleased] — 1.8.0

### Added

- **Web Push / VAPID** — full background push notifications, no open tab required.
  - ECDH P-256 VAPID key pair auto-generated on first start, persisted to
    `vapid_key_file` (default `vapid.json`, mode `0600`).
  - New config keys: `[notifications] webpush = false` (master switch) and
    `vapid_key_file = "vapid.json"`.
  - Server routes (registered only when `webpush = true`):
    - `GET /api/push/vapid-public` — public endpoint returning the base64url
      VAPID public key for use as `PushManager.subscribe({ applicationServerKey })`.
    - `POST /api/push/subscribe` — upserts a browser `PushSubscription` JSON blob
      for the authenticated user (stored in per-user bbolt `push.db`).
    - `DELETE /api/push/subscribe` — removes a subscription by endpoint URL.
  - `PushStore` (`handlers/web/pushstore.go`) — bbolt-backed per-user subscription
    store; upsert, delete, list-all; isolates subscriptions by username.
  - `LoadOrGenerateVAPIDKeys` (`handlers/web/vapid.go`) — loads or generates the
    VAPID key pair; gracefully regenerates on corrupt file.
  - `SendPush` (`handlers/web/push.go`) — fan-out delivery via
    `SherClockHolmes/webpush-go`; expired (HTTP 410) subscriptions auto-removed;
    called from `NotificationHub.Broadcast` in a background goroutine.
  - Service worker **`/sw.js`** — served at root scope with `Cache-Control: no-cache`
    and `Service-Worker-Allowed: /`; handles `push` (shows notification),
    `notificationclick` (focuses existing LilMail tab or opens new one), and
    `pushsubscriptionchange` (re-subscribes and POSTs new subscription).
  - Client-side `window.lilmailPush` API — `enable()`, `disable()`, `isSupported()`,
    `isSubscribed()` — injected into every page when `webpush = true`.
  - **Settings page** (`GET /settings`, template `templates/settings.html`) — always
    registered; shows Web Push toggle when `webpush = true`; account management
    when `accounts.enabled = true`.
  - Settings gear icon (⚙) in the top bar linking to `/settings`.
  - New template funcs: `webPushEnabled()`, `accountsEnabled()`.
  - Tests: `handlers/web/push_test.go` covers key generation, load, corrupt-file
    recovery, PushStore CRUD (save, delete, upsert, multi-user isolation,
    multiple subscriptions), and push payload JSON correctness + size guard.

- **Multiple accounts / account switcher** — add and switch between IMAP/SMTP
  accounts without logging out.
  - New config section `[accounts]` with `enabled` (master switch, default `false`)
    and `store_file` (bbolt path, default `accounts.db`).
  - `AccountStore` (`handlers/web/accountstore.go`) — per-primary-user bbolt store
    for additional mail accounts; each entry stores email, label, colour badge,
    IMAP/SMTP host/port, and the AES-256-GCM–encrypted password (same key as
    session credentials). CRUD: `Save`, `Delete`, `List`.
  - `AccountsHandler` (`handlers/web/accounts.go`) — HTTP handlers:
    - `GET /api/accounts` — lists additional accounts (passwords stripped from response).
    - `POST /api/accounts` — validates credentials against IMAP, encrypts password,
      stores in `AccountStore`. Defaults IMAP/SMTP to global config when not specified.
    - `DELETE /api/accounts/:email` — removes an account.
    - `POST /api/accounts/:email/switch` — replaces the session identity with the
      target account (re-validates credentials); saves the previous identity as an
      additional account under the new owner so switching back works immediately.
    - `GET /settings` — renders the settings page.
  - All account routes gated on `[accounts] enabled = true`.
  - Tests: `handlers/web/accountstore_test.go` covers CRUD, upsert, multi-owner
    isolation, empty list, and persistence across re-opens.

### Changed

- `NewNotificationHub` signature gains two optional parameters (`vapidKeys *VAPIDKeys`,
  `pushStore *PushStore`) — both `nil` when `webpush = false`; no behaviour change
  for existing SSE-only configurations.
- `NotificationsConfig` gains `WebPush bool` and `VAPIDKeyFile string` fields;
  defaults: `false` / `"vapid.json"`.
- `Config` gains `Accounts AccountsConfig` field.
- `tmpl_smoke_test.go` registers `webPushEnabled` and `accountsEnabled` stubs so
  the template smoke test stays green.
- `config.toml.example` documents all new config keys.

### Dependencies

- Added `github.com/SherClockHolmes/webpush-go v1.4.0` for VAPID key generation
  and RFC 8291/8292 push message encryption.

---

## [1.7.0] — 2026-06-15

### Added

- **Drafts** — Save drafts via `POST /api/draft` which appends the message to
  the IMAP Drafts folder (discovered by `\Drafts` special-use attribute via
  `IMAP LIST`, with name-guess fallback). The compose modal gains a "Draft"
  button and auto-saves every 30 s while composing. Clicking a draft in the
  Drafts folder opens it back into compose (To/CC/Subject/Body/HTML all
  restored); sending a composed draft deletes the old draft from IMAP via
  `UID STORE +FLAGS \Deleted` + EXPUNGE. Route: `GET /api/drafts` for the
  draft list partial; `POST /api/draft` for save; `POST /api/compose` with
  `draft_uid` for replace-and-send.

- **Attachments in compose** — The compose form uses `enctype=multipart/form-data`
  and a multi-file `<input type=file>`. The server builds a proper
  `multipart/mixed` MIME message (body part + one attachment part per file,
  each base64-encoded with RFC 2045-compliant 76-char line breaks and correct
  `Content-Type` / `Content-Disposition` headers). The same raw bytes are sent
  via SMTP and APPENDed to the Sent folder so the Sent copy is complete.
  Selected filenames are listed in the compose modal before sending.

- **HTML compose** — Compose modal gains a "Rich/Plain" toggle. Rich mode shows
  a `contenteditable` editor with a lightweight dependency-free formatting
  toolbar (bold, italic, underline, strikethrough, ordered/unordered lists,
  link, remove formatting — all via `document.execCommand`). Toggling back to
  plain copies text from the editor. On send/draft-save the HTML is placed in
  a `text/html` part and the plain text in `text/plain`; both are wrapped in
  `multipart/alternative` (plain first per RFC 2046). Existing plain-text
  compose continues to work unchanged.

- **Recipient autocomplete** — To/CC/BCC inputs now show an inline autocomplete
  dropdown. Selecting an entry appends it to the comma-separated field.
  Two data sources:
  - **Recent recipients** — every address in the To/CC fields of a sent message
    is recorded (email, display name, send count, last-used time) in the shared
    per-user bbolt database. Count and recency drive sort order.
  - **CardDAV contacts** (optional) — when `[carddav] enabled = true` in
    `config.toml`, LilMail queries the configured address book via a
    `carddav.AddressBookQuery` and merges matching vCard `FN`/`EMAIL` fields
    into the suggestions list. Requires no additional dependency — uses the
    transitive `go-webdav`/`go-vcard` already present.
  Route: `GET /api/autocomplete?q=<query>` (JSON array of `{email, name}`).

- New config section `[carddav]` (`enabled`, `url`, `username`, `password`) for
  CardDAV address-book contact queries (independent of the `[caldav]` calendar
  integration).

- **Central MIME builder** (`handlers/api/mime_builder.go`) — single function
  `BuildMIMEMessage` produces correct RFC 2822 + MIME messages for all paths
  (send, draft save, sent-folder copy). Handles plain, alternative, mixed, and
  mixed-with-alternative combinations. Quoted-printable body encoding.

- **`SendRawMessage`** on `SMTPClient` — takes pre-built message bytes and a
  list of envelope recipients; share the same SMTP connection logic as
  `SendMail`.

- **Tests** — `handlers/api/mime_builder_test.go` covers plain, HTML+plain,
  attachments, mixed+alternative, threading headers, empty body.
  `handlers/api/recipients_test.go` covers Record/Search, count increment,
  sort-by-count, limit, name update, persistence, and last-used timestamp.

### Changed

- `Client.SaveToSent` now accepts `rawMessage []byte`; when non-nil the exact
  bytes are APPENDed (so the Sent copy matches what was actually sent). Falls
  back to a synthetic plain-text message for backwards compatibility.
- `HandleComposeEmail` now builds the MIME message via `BuildMIMEMessage` and
  sends via `SendRawMessage`; the original `SendMail` path is preserved for
  programmatic callers.

---

## [1.6.0] — 2026-06-01

### Added

- **Mark-as-unread** — the "Mark as unread" dropdown item in the email viewer
  now fires a real `PATCH /api/email/:id/unread` request that removes `\Seen`
  via `IMAP UID STORE`.  The email list refreshes automatically.
- **Search** — the top-bar search box is wired to `GET /api/search?q=…` which
  performs an `IMAP UID SEARCH TEXT` query and returns a live email-list partial.
  Results appear while typing (500 ms debounce + native search event).
- **Reply/Forward threading** — compose now sends `In-Reply-To` and `References`
  headers when replying so clients thread the conversation correctly (RFC 2822
  §3.6.4). The reply button populates hidden `in_reply_to` and `references`
  form fields automatically.
- **CC/BCC** — compose modal has collapsible CC and BCC fields; both are
  submitted to the SMTP send path and wired as proper `RCPT TO` envelopes
  (BCC is envelope-only, not added to headers).
- **SMTP implicit TLS (port 465)** — `SMTPClient` now honours
  `smtp.use_starttls = false` by using `tls.Dial` (implicit TLS) instead of
  the plain-TCP + STARTTLS upgrade path.  Default remains STARTTLS (port 587).
- **Sent-folder discovery** — `SaveToSent` now uses `IMAP LIST` to discover
  the real Sent folder by the `\Sent` special-use attribute before falling back
  to common name guesses.
- **Real iTIP RSVP** — `POST /calendar/rsvp` now builds a `METHOD:REPLY`
  iCalendar payload and delivers it to the event organiser via the session
  SMTP client (RFC 5546).  No more fake-success stub.
- **`[server] secure_cookies`** config key — set to `true` in
  TLS-terminated deployments to add the `Secure` flag to session cookies.
  Defaults to `false` for plain-HTTP local dev.
- **Shared bbolt handle** — `EmailHandler` now opens one bbolt thread-cache
  file per user and reuses it across requests; previously a new file handle was
  opened (and locked) on every inbox load.
- Handler tests for `handlers/web/` covering threading, `MailOptions`, and
  mark-unread wiring.

### Changed

- `config.toml` renamed to `config.toml.example` (added to `.gitignore`) so
  placeholder secrets are never committed.  Copy it to `config.toml` to run.
- `strings.Title` (deprecated) replaced by a local `titleCase` helper.
- `io/ioutil` (deprecated) replaced with `io`/`os` equivalents throughout.
- `SMTPClient` constructors take an explicit `useStartTLS bool` parameter
  (previously always STARTTLS).

### Security

- **CRITICAL: srcdoc XSS fixed** — `email.HTML` was interpolated as
  `template.HTML` directly into the `srcdoc="..."` attribute of the sandboxed
  iframe; a quote character in a malicious email body could break out of the
  attribute and execute script in LilMail's origin. The value is now auto-escaped
  as a plain Go template string so HTML is passed verbatim without interpretation.
- **Full Content-Security-Policy** — `script-src`, `style-src`, `img-src`,
  `connect-src`, `object-src`, `base-uri`, and `frame-ancestors` now emitted on
  every response. `X-Frame-Options` is omitted when `frame_ancestors` is set
  (only the CSP variant supports an allow-list).
- **`SameSite=Lax` session cookie** — explicitly set to prevent CSRF via
  cross-site form submissions.

### Added

- **AI mail assistant** (`[ai]` config section) — opt-in, disabled by default.
  Five endpoints added under `/api/ai/`:
  - `POST /api/ai/compose` — smart compose / continue / rewrite
  - `POST /api/ai/summarize` — thread summary + key points + action items
  - `POST /api/ai/reply` — three reply suggestions (concise / detailed / decline)
  - `POST /api/ai/extract-actions` — action items with optional due dates
  - `POST /api/ai/phishing` — phishing / suspicious / clean classification

  Calls a configurable OpenAI-compatible SSE chat-completion endpoint. Default
  endpoint targets the Vulos OS airouter; any compatible provider works for
  standalone use. Mail content is forwarded and discarded — never persisted.
  Prompt-injection guard applied to all user-supplied strings before substitution
  into prompt templates. Tests in `handlers/ai/ai_test.go`.

- **`[server] frame_ancestors`** config key — space-separated CSP
  `frame-ancestors` value. When set, LilMail can be embedded as an iframe by the
  listed origins (e.g. the Vulos OS shell). Defaults to `'self'` (same-origin
  only). Config test coverage added.

### Changed

- **UI: hand-written CSS design system** — replaced Tailwind CDN with a
  single `assets/css/mail.css` stylesheet (~20 KB). All twelve templates
  restyled: 3-pane layout, dark mode, Gmail-like density, improved login page,
  calendar views, compose modal, toast notifications. Tailwind vendor file
  (`assets/vendor/tailwind.js`) is still embedded but no longer loaded.
- **Security headers applied on every response** — `GetSecurityHeaders()` is
  now wired as a middleware in `main.go`; previously it was defined but never
  called.
- **Collapsed encrypt/decrypt API** — three AES-GCM encrypt/decrypt pairs
  unified to a single `EncryptJSON` / `DecryptJSON` generic pair. On-wire
  format unchanged; existing encrypted blobs remain readable. Tests in
  `handlers/api/auth_test.go`.
- **Removed duplicate `/htmx/*` routes** — templates only call `/api/*`; the
  redundant `/htmx/` route group has been removed from `main.go`.
- Debug `fmt.Printf` / `log.Println` calls (login username, folder fetch count,
  cache-clear) replaced with structured `log.Printf` or removed.

### Fixed

- `GetSecurityHeaders()` was defined in `config/config.go` but never invoked;
  the application was not emitting any security headers. Now applied via
  middleware before every response.

---

## [1.4.0] — 2026-05-24

### Added

- **OAuth2 / OpenID Connect** login with XOAUTH2 and OAUTHBEARER SASL for IMAP
  and SMTP — authorization-code flow, PKCE, automatic refresh-token handling.
- **Attachment download** — metadata read from `BODYSTRUCTURE`; content fetched
  on demand per MIME part (base64 / quoted-printable decoded) and streamed via
  `GET /api/attachment/:id`.
- **Gmail-inspired responsive UI** — sticky top bar, collapsible sidebar,
  docked compose modal, sandboxed HTML mail iframe, mobile drawer (Alpine.js).
- **JWZ conversation threading** — `References` / `In-Reply-To` / `Message-ID`
  grouping backed by an embedded bbolt store; collapse/expand UI.
- **CalDAV calendar** — month/week views, event creation, iCalendar invite
  detection with basic RSVP affordance. Opt-in via `[caldav].enabled`.
- **Real-time notifications** (Phase 6) — IMAP IDLE watcher, SSE stream →
  Web Notifications API, opt-in native desktop toasts via `gen2brain/beeep`.
  Opt-in via `[notifications].enabled`. Web Push deferred (see ROADMAP).
- **Self-contained binary** — templates and vendor JS (HTMX, Alpine.js) embedded
  via `embed.FS`; runs fully offline with only `config.toml`.
- **CI/CD** — `ci.yml` (build + vet + test on push/PR) and `release.yml`
  (multi-platform archives on `v*` tags) GitHub Actions workflows.
- **Security hardening** — path-safe cache (username sanitized), `0700`/`0600`
  file permissions, atomic cache writes, SMTP TLS verification, `BodyLimit`.
- Unit tests: attachment-ID codec, `SanitizeUsername`, SMTP SASL, MIME decode,
  JWZ threading.

---

## [1.0.7] and earlier

Initial releases: basic IMAP/SMTP webmail, JWT sessions, file-based cache,
password-only login, server-rendered Go templates.

[Unreleased]: https://github.com/exolutionza/lilmail/compare/v1.7.0...HEAD
[1.7.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.7.0
[1.6.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.6.0
[1.4.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.4.0
