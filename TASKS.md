# LilMail Tasks 📋

Working backlog derived from [ROADMAP.md](ROADMAP.md). Checked items are done;
phases are executed in order. Each task must leave `go build ./...`,
`go vet ./...`, and `gofmt -l` clean, and must preserve the existing
existing design system (hand-written CSS + Alpine.js).

Stack reminder: Go + Fiber, server-rendered Go templates + HTMX + Alpine.js +
hand-written CSS (dark mode; `assets/css/mail.css`), file-based cache, no
external DB. Mail via `emersion/go-imap` and `net/smtp`.

---

## Phase 0 — Shipped ✅

- [x] OAuth2 / OIDC login (XOAUTH2 + OAUTHBEARER) for IMAP & SMTP, with PKCE and
      refresh-token handling
- [x] Attachment download (metadata from BODYSTRUCTURE; on-demand part fetch +
      base64/quoted-printable decode; `GET /api/attachment/:id`)
- [x] MIT license; polished README (Vulos OS); ROADMAP
- [x] Hardened release pipeline (`name`, permissions, setup-go@v5 / Go 1.23 /
      action-gh-release@v2)

## Phase 1 — Scale & security 🔒

- [x] **1.1** Add `SanitizeUsername`/safe-join helper in `handlers/api`; use it
      everywhere the username becomes a cache path (`web/auth.go` login, logout,
      `CreateIMAPClient`, `oauth.go` callback). Reject/escape `/`, `\`, `..`,
      control chars.
- [x] **1.2** Tighten cache file perms (0600 for `emails.json`/`folders.json`,
      0700 for the user dir); ensure writes are atomic (temp + rename) in
      `utils` cache helpers.
- [x] **1.3** SMTP TLS: drop the hardcoded `InsecureSkipVerify: true`; verify
      against the server name. Add `[smtp] insecure_skip_verify` (default
      `false`) for self-signed servers.
- [x] **1.4** Stream large attachments (use `c.SendStream` / chunked copy) and
      enforce a configurable max attachment size; add Fiber `BodyLimit` in
      `main.go`.
- [x] **1.5** Unit tests: attachment-ID codec, `SanitizeUsername`, SMTP SASL
      Start() output, MIME `decodeContent`.

## Phase 2 — UI/UX, Gmail-inspired & responsive 🎨

- [x] **2.1** `templates/layouts/main.html`: responsive app shell — sticky top
      bar (logo, search, account), collapsible sidebar that becomes a mobile
      drawer (Alpine). Keep current theme/colors.
- [x] **2.2** `templates/inbox.html` + `partials/email-list.html`: Gmail-like
      density, unread emphasis, row hover actions (archive/delete/read),
      responsive. Fix broken non-INBOX folder navigation.
- [x] **2.3** `partials/email-viewer.html`: cleaner header/avatar, responsive
      layout, render HTML mail safely in a sandboxed `<iframe srcdoc>` (or
      server-side sanitize), keep attachment chips wired to
      `/api/attachment/:id`.
- [x] **2.4** `partials/compose-modal.html`: Gmail-style docked compose
      (bottom-right card; full-screen on mobile); wire To/Subject/Body + send.
- [x] **2.5** Rendering-bug pass: populate list **preview** text (fetch a small
      body snippet in `FetchMessages`), fix `toast.html`, loading states, and
      any broken `{{}}`/HTMX targets.

## Phase 3 — JWZ threading 🧵

- [x] **3.1** Add `go.bbolt` (`go.etcd.io/bbolt`) dependency; open a per-user
      bolt file under the cache folder.
- [x] **3.2** Fetch threading headers (`Message-ID`, `In-Reply-To`,
      `References`) in `FetchMessages`; add fields to `models.Email`.
- [x] **3.3** Implement the **JWZ** algorithm in `handlers/api/threading.go`
      (id_table, link by references, prune empty containers, group by subject).
      Pure Go, efficient. Unit-tested with sample headers.
- [x] **3.4** Persist/refresh a thread index in bolt; expose
      `BuildThreads(folder)` returning conversation groups.
- [x] **3.5** Conversation UI in the list (collapse/expand, message count,
      latest snippet) + a thread view route/handler.

## Phase 4 — Calendar (CalDAV) 📅

- [x] **4.1** `[caldav]` config block (enabled, url, auth: `oauth2`|`basic`,
      username/password). Add `emersion/go-webdav` (CalDAV) + `emersion/go-ical`.
- [x] **4.2** CalDAV client in `handlers/api/caldav.go`: discover calendars,
      list events in a date range, parse iCal, create a basic event.
- [x] **4.3** Routes + handlers: `/calendar` (month view), `/calendar/week`,
      event detail, create-event POST.
- [x] **4.4** Gmail-style calendar templates (month grid + week view) on the
      existing theme; sidebar/nav link guarded by `[caldav].enabled`.
- [x] **4.5** iCalendar (`.ics`) invite detection in the mail viewer with a
      basic RSVP affordance.

## Phase 6 — Notifications & real-time 🔔

- [x] **6.1** `[notifications]` config (enabled, `desktop` opt-in for native
      toasts, `idle` to enable the IMAP IDLE watcher).
- [x] **6.2** IMAP IDLE watcher (add `emersion/go-imap-idle`): per-session
      goroutine watching INBOX for new messages; emit a "new mail" event with
      sender/subject. Stop cleanly on logout/session end.
- [x] **6.3** SSE endpoint (`GET /events`) that streams new-mail events to the
      browser; client JS subscribes and raises `Notification(...)` via the Web
      Notifications API (request permission on first login). Works while a tab
      is open.
- [x] **6.4** Opt-in **native desktop** notifications via `gen2brain/beeep`
      (config `notifications.desktop`) for local/desktop runs.
- [ ] **6.5** (stretch) Web Push: VAPID keys in config, a Service Worker, and a
      subscription endpoint for background notifications. DEFERRED — see
      `TODO(webpush)` comment in `handlers/web/notifications.go` and the
      "Web Push (VAPID + Service Worker)" entry in ROADMAP.md.

## Phase 5 — Self-contained & CI 📦 (run LAST so it captures all assets)

- [x] **5.1** Embed `templates/` (and vendored HTMX/Alpine.js + CSS +
      notification Service Worker / JS) via `embed.FS`; serve assets locally so
      the binary runs offline.
- [x] **5.2** Add a `ci.yml` workflow: `go build` + `go vet` + `go test ./...`
      on push/PR.
- [x] **5.3** Update README/config docs for new `[smtp]`, `[caldav]`,
      `[notifications]` options and the calendar/threading/notification features.

---

### Execution notes (for agents)

- Touch only the files your task names; don't refactor unrelated code.
- Always finish with: `gofmt -w` your files, then `go build ./... && go vet ./...`.
- Preserve existing routes, session model, and the OAuth2/password auth split.
- Keep the current visual theme (`assets/css/mail.css`); "Gmail-inspired" means
  layout/interaction, not a color-scheme replacement.
- When adding a dependency, run `go mod tidy` and confirm the build.
