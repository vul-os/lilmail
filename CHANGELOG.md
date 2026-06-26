# Changelog

All notable changes to LilMail are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)  
Versioning: [Semantic Versioning](https://semver.org/)

---

## [Unreleased]

### Added

- **JSON API (`/v1`)** — a clean JSON/REST surface served alongside the HTMX UI,
  for rich React clients (Vulos Mail, Vulos Workspace) and scripting. Endpoints:
  `GET /v1/me`, `GET /v1/folders`, `GET /v1/messages`, `GET /v1/messages/:uid`,
  `GET /v1/search`, `PATCH /v1/messages/:uid/flags`, `DELETE /v1/messages/:uid`.
  It reuses the existing mail engine and session auth (no duplicated mail logic),
  folder names ride as the `folder` query param, and unauthenticated requests get
  `401` JSON instead of an HTML redirect. New package `handlers/jsonapi`; the HTMX
  UI is untouched. Documented in [docs/API.md](docs/API.md).
- **JSON API compose, calendar & contacts (`/v1`)** — additive endpoints over the
  same engine as the HTMX surfaces: `POST /v1/messages` (send) and `POST /v1/drafts`
  (save draft) build messages with `api.BuildMIMEMessage` and send via the existing
  SMTP client; when `[caldav] enabled`, `GET/POST /v1/calendar/events`,
  `DELETE /v1/calendar/events/:uid` and `GET /v1/calendar/freebusy` reuse the CalDAV
  client and `models.Calendar*` types; when `[carddav] enabled`, `GET /v1/contacts`
  reuses the CardDAV query path. CalDAV client construction is now shared via
  `AuthHandler.CalDAVClient`, and `CalDAVClient` gained `DeleteEvent` + `FreeBusy`
  helpers. Documented in [docs/API.md](docs/API.md).
- **Optional Postgres storage backend** — a new durable key-value seam
  (`storage/` `KV` interface) with two backends: the embedded **bbolt** store
  (default — keeps lilmail a single binary with nothing to run) and an optional
  **Postgres** store for shared / multi-instance deploys, selected via the new
  `[storage]` config section (`backend`, `postgres_dsn`). Postgres is strictly
  opt-in; the schema auto-creates on first connect. Lets other Vulos services
  share the same store. Documented in [docs/CONFIGURATION.md](docs/CONFIGURATION.md#storage).

---

## [1.10.0] — 2026-06-22

### Added

- **Full email address as login username** — new `[auth] allow_full_email_username`
  config key controls what LilMail sends as the IMAP/SMTP SASL/LOGIN username:
  - `true` (default) — the full email address (`alice@example.com`) is sent
    verbatim, matching most hosted providers (Gmail, Fastmail, Migadu, Zoho…).
  - `false` — only the local part before `@` (`alice`) is sent, for self-hosted
    Dovecot/Postfix setups that authenticate the bare handle.
  - The legacy `[server] username_is_email` key is kept as a backwards-compatible
    alias; when `[auth] allow_full_email_username` is set it takes precedence.
    `LoadConfig` reconciles the two into a single source of truth
    (`Server.UsernameIsEmail`) that all auth paths (password login, OAuth2,
    additional accounts, account switch, SMTP) already read.
  - Documented in `config.toml.example`; covered by
    `config.TestAllowFullEmailUsername_AuthSectionWins` (default, auth-only,
    legacy-only, and auth-overrides-legacy cases).

### Changed

- **New brand identity — Coral & Teal warm-paper palette.** The whole design
  system was retinted from the previous indigo scheme onto a coral primary
  (`#F2674E`), teal link/highlight (`#14B8A6`), ink/slate text, and warm
  "paper" surfaces (`#FBF7F4`) with mist borders (`#EEE6E0`). All `--c-*` design
  tokens in `assets/css/mail.css` were remapped (light + a derived warm-charcoal
  dark variant), avatar and status colours re-harmonised, and every hardcoded
  indigo value removed from templates and CSS. WCAG-AA text contrast preserved.
- **New logo, favicons & social meta.** The coral envelope-flap mark replaces the
  old logo in the topbar and login card and is used as the favicon. Generated
  app-icon PNGs (16/32/48/180/192/512) plus an Open Graph image; completed
  `<head>` with description, `theme-color`, `color-scheme`, Open Graph, and
  Twitter card meta; web manifest references the new icons.
- **Thunderbird-class UI/UX overhaul.** The inbox is now a true resizable
  three-pane layout — collapsible account/folder tree with unread counts (and the
  unified-inbox toggle), a message list with avatars, threaded/collapsible
  conversations, unread emphasis, multi-select + bulk action bar, and a
  comfortable/compact density toggle — alongside a reading pane (right/bottom/off,
  draggable divider, sizes persisted) with a full header block, avatar, attachment
  chips, the sandboxed HTML body (XSS sanitisation unchanged), and a complete
  action toolbar. Reply / Reply-All / Forward / Mark-unread / Delete / Print are
  wired to existing handler routes; Archive / Junk are laid out and clearly flagged
  as awaiting backend IMAP-move support. Keyboard navigation (j/k, Enter/o, r, u,
  Delete, c, `/`, Esc) added. Login, Settings, Calendar, the compose modal, and
  the error page were all brought up to the new palette and polish, with refined
  loading/empty/hover/focus states and a single-pane responsive layout on mobile.

### Docs

- README now leads with a hero screenshot of a message open in the three-pane
  reading view; demo-mode screenshot pipeline regenerated all screenshots.

---

## [1.9.0] — 2026-06-16

### Added

- **Unified inbox** — multiplexed cross-account inbox fetch and combined view.
  - `FetchUnified` (`handlers/web/unified.go`) — fans out to the session account
    and every additional account concurrently (one goroutine per account). Each
    goroutine runs with a 10 s `context.WithTimeout`; one account timing out or
    failing does **not** affect the others. Results are merged and sorted by date
    descending, capped at `min(200, 50 × accounts)` messages.
  - `AccountFetchResult` / `AccountFetchError` — per-account result type that
    carries the fetched emails (tagged with source account metadata) or the error
    for that account. Callers can inspect `HasErrors()` to show a per-account
    warning without losing messages from healthy accounts.
  - `models.Email` gains three new optional fields: `AccountEmail`, `AccountLabel`,
    `AccountColor`. These are empty in single-account mode; no template or API
    change is visible to users who don't use unified mode.
  - `AuthHandler.CreateIMAPClientForAccount` / `CreateSMTPClientForAccount` —
    open IMAP/SMTP connections for a stored `AccountEntry` by decrypting its
    password exactly as the rest of the app does.
  - `EmailHandler.SetAccountStore` — late-wire the `AccountStore` after it is
    opened in `main.go`; keeps `NewEmailHandler` signature unchanged.
  - **UI toggle** — when `[accounts] enabled = true` and at least one additional
    account is stored, a "Unified" pill appears in the inbox list toolbar.
    Activating it navigates to `/inbox?unified=1`; deactivating returns to
    `/inbox`. The HTMX folder-switch partial (`/api/folder/INBOX/emails?unified=1`)
    also accepts the `?unified=1` flag so the toggle works without a full page
    reload.
  - **Account badge** — each email row in unified mode shows the source account's
    label as a coloured pill using the account's configured badge colour.
  - **Per-account error indicators** — when one or more accounts fail to load,
    a red dot with a tooltip appears in the toolbar (one per failed account).
    Successfully loaded accounts are unaffected.
  - **Correct account for view / reply / send** — clicking a message in unified
    mode passes `X-Account-Email` in the HTMX request headers; the server opens
    an IMAP connection for that specific account to fetch the full message.
    Replying/composing passes `account_email` as a hidden form field; the server
    uses that account's SMTP credentials (and IMAP for Sent-folder append) for
    the send.
  - **Graceful degradation** — when `[accounts] disabled` or only one account
    exists, the toggle is hidden and all behaviour is identical to before.
  - **Tests** (`handlers/web/unified_test.go`) — 8 tests covering: merge order
    (newest-first across accounts), per-account error isolation (one failure does
    not suppress others), all-failed case (empty output), limit cap,
    account-tag preservation, single-account equivalence, `AccountFetchError`
    `HasErrors()` / `Error()`, and empty-input safety.

### Changed

- `EmailHandler.HandleInbox` — detects `?unified=1` and fans out; passes
  `Unified`, `UnifiedAvailable`, and `AccountErrors` to the template.
- `EmailHandler.HandleFolderEmails` — same flag, scoped to `INBOX` only.
- `EmailHandler.HandleEmailView` — reads `X-Account-Email` header to route the
  IMAP fetch to the correct account; falls back to the session account.
- `EmailHandler.HandleComposeEmail` — reads `account_email` form field; uses
  that account's SMTP client and IMAP client (for Sent-folder APPEND).
- `inbox.html` — toolbar gains unified toggle and error-indicator slots;
  `email-rows` sub-template renders `acct-badge` in unified mode and passes
  `X-Account-Email` in HTMX headers.
- `partials/email-list.html` — same badge + header changes as `email-rows`.
- `assets/css/mail.css` — adds `.acct-badge`, `.unified-toggle`,
  `.unified-toggle--active`, `.list-toolbar__unified`, `.list-toolbar__acct-errors`,
  `.acct-error-dot` styles.
- `config.toml.example` — documents the unified inbox behaviour under `[accounts]`.

---

## [1.8.0] — 2026-06-15

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

[Unreleased]: https://github.com/exolutionza/lilmail/compare/v1.10.0...HEAD
[1.10.0]: https://github.com/exolutionza/lilmail/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/exolutionza/lilmail/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/exolutionza/lilmail/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.7.0
[1.6.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.6.0
[1.4.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.4.0
