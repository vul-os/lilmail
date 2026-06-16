# Architecture

lilmail is a server-rendered Go web application using the
[Fiber](https://gofiber.io/) HTTP framework. All frontend assets and HTML
templates are embedded in the binary at build time via `embed.FS` — no static
file server or separate asset pipeline is required.

## Repository layout

```
lilmail/
├── main.go                  # Entry point: config, DI wiring, route registration
├── config/
│   ├── config.go            # Config structs + TOML loader
│   └── config_test.go
├── handlers/
│   ├── ai/                  # AI mail assistant endpoints
│   ├── api/                 # JSON API handlers (email, attachments, search, …)
│   └── web/                 # HTML page handlers (inbox, viewer, settings, …)
├── models/
│   ├── email.go             # Email, Attachment, Thread model types
│   └── calendar.go          # CalDAV event types
├── storage/
│   └── session.go           # bbolt-backed session/credential store
├── sessions/                # Runtime session state (file-based)
├── utils/
│   └── cache.go             # On-disk cache helpers
├── templates/               # Go HTML templates
│   ├── layouts/main.html    # Shared app shell (nav, sidebar, top bar)
│   ├── partials/            # HTMX partial fragments (email-list, compose, …)
│   ├── inbox.html
│   ├── login.html
│   ├── settings.html
│   ├── calendar.html
│   └── calendar-week.html
├── assets/
│   ├── css/mail.css         # Hand-written CSS (dark mode, responsive)
│   ├── vendor/              # htmx.min.js, alpine.min.js, tailwind.js
│   └── sw.js                # Service worker (Web Push)
├── scripts/                 # Developer tooling (Playwright screenshotter)
├── docs/                    # Documentation and screenshots
├── .github/workflows/       # CI + release pipelines
├── config.toml.example
├── go.mod / go.sum
└── tmpl_smoke_test.go       # Template parse smoke test
```

## Request lifecycle

```
Browser
  │
  ▼
Fiber HTTP server (main.go)
  │
  ├─ Middleware: JWT session auth (except /login, /health, /sw.js)
  │
  ├─ Web routes  → handlers/web/   → Go templates → HTML response
  │
  ├─ API routes  → handlers/api/   → JSON response
  │                  │
  │                  └─ IMAP/SMTP clients (created per request from session creds)
  │
  ├─ AI routes   → handlers/ai/    → proxies to configured SSE endpoint
  │
  └─ SSE route   → IMAP IDLE watcher → SSE stream → browser Web Notifications
```

## Key subsystems

### Authentication

`handlers/web/auth.go` (and `handlers/web/oauth.go`) handle login/logout.
Credentials are encrypted with AES-256-GCM (`[encryption].key`) before being
stored in the JWT session. The JWT is signed with `[jwt].secret` and stored in
a `SameSite=Lax` HTTP-only cookie (`Secure` when `[server].secure_cookies =
true`).

OAuth2: lilmail runs the full authorization-code flow with PKCE. After callback,
the access + refresh tokens are encrypted and stored in the session exactly like
passwords. Token refresh happens transparently on the next IMAP/SMTP operation
that receives a 401/NO AUTHENTICATE.

### IMAP / SMTP

`handlers/api/email.go` creates IMAP clients from session credentials on each
request using `emersion/go-imap`. There is no persistent connection pool —
connections are opened, used, and closed per request. This keeps memory usage
low and makes the server stateless with respect to IMAP state.

SMTP sending lives in `handlers/api/compose.go` (and related files). The SASL
mechanism (plain, XOAUTH2, or OAUTHBEARER) is chosen based on `[oauth2].mechanism`.

### Caching

Fetched email metadata is written to the on-disk cache (`[cache].folder`) as
JSON files, one per folder. MIME bodies are cached separately. Cache keys are
based on the sanitized username and folder name (path-traversal-safe).

### Conversation threading

`storage/session.go` wraps a shared bbolt database per user (one file per
session identity). Thread graphs are built using the JWZ algorithm over
`Message-ID`, `References`, and `In-Reply-To` headers and stored in bbolt.

### Notifications

When `[notifications].enabled = true`, a per-session IMAP IDLE goroutine is
started after login. New-mail events are pushed to a channel that drives an SSE
response on `GET /events`. The browser Web Notifications API is triggered from
client-side JavaScript listening to the SSE stream.

Web Push uses the `SherClockHolmes/webpush-go` library. VAPID keys are
auto-generated on first start and persisted to `vapid.json`.

### CalDAV / CardDAV

`handlers/web/calendar.go` uses `emersion/go-webdav` + `emersion/go-ical` for
CalDAV and `emersion/go-vcard` for CardDAV. Both are purely opt-in: routes and
goroutines are only registered when the respective config section has
`enabled = true`.

### AI assistant

`handlers/ai/` proxies requests to a configurable OpenAI-compatible
chat-completion endpoint. Prompts live in `handlers/ai/prompts/*.txt` and are
loaded at startup. No mail content is persisted by lilmail; it is forwarded and
discarded.

### Frontend

The UI is server-rendered Go templates enhanced with
[HTMX](https://htmx.org/) (partial page updates) and
[Alpine.js](https://alpinejs.dev/) (inline interactivity). All vendor JS is
checked in under `assets/vendor/` — there is no npm or build step. CSS is
hand-written in `assets/css/mail.css` (~52 KB) with a dark-mode theme.

## Build and embedding

`go build ./...` produces a single self-contained binary. `//go:embed` directives
in `main.go` embed `templates/`, `assets/`, and `handlers/ai/prompts/` into the
binary at compile time. The binary can run fully air-gapped without any companion
files except `config.toml`.

## CI / release

`.github/workflows/ci.yml` — runs `go build`, `go vet`, and `go test ./...` on
every push.

`.github/workflows/release.yml` — triggered on `v*` tags; cross-compiles for
Linux/macOS/Windows (amd64), packages archives, and publishes to GitHub Releases.
