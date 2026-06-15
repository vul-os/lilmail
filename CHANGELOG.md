# Changelog

All notable changes to LilMail are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)  
Versioning: [Semantic Versioning](https://semver.org/)

---

## [Unreleased] — v1.6.0

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

[Unreleased]: https://github.com/exolutionza/lilmail/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/exolutionza/lilmail/releases/tag/v1.4.0
