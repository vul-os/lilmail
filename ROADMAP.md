# LilMail Roadmap 🗺️

LilMail is an **independent, self-hostable PIM client** — mail, calendar, and
contacts — for the web: a resource-light alternative to Thunderbird in a single
Go binary, with no database to administer and a UI that stays out of your way. It
talks to the user's own IMAP/SMTP + CalDAV + CardDAV accounts and exposes a stable
`/v1` JSON API; the Vulos OS integrates it over that seam (it is not a hosted
service). This document tracks where we are and where we're going.

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
| 🔜 | Planned (near-term) |
| 💭 | Exploratory / longer-term |

---

## ✅ Shipped

**Core mail**
- IMAP folder browsing and message viewing (correct transfer-decoding and
  single-part `text/html` rendering); SMTP sending + save-to-Sent
- File-based caching, no database required
- JWT sessions + AES-GCM encrypted credentials/tokens at rest
- Full-email-as-username support (`allow_full_email_username`)

**JSON API (`/v1`)**
- A stable REST surface (folders, paginated messages, search, flags,
  move/delete, compose + drafts, scheduled send, calendar, contacts, settings)
  served alongside the HTMX UI from the same engine and session auth — the
  contract the Vulos OS and other rich clients build on. See
  [docs/API.md](docs/API.md).
- **Send-as identities** — `GET`/`PUT /v1/settings/identities`; compose,
  draft-save, and scheduled send all honour and enforce the chosen `From`.
- **Optional Postgres storage backend** for shared / multi-instance deploys
  (bbolt embedded store remains the zero-config default).
- **Injected-credential ("brokered") mode**, off by default — an embedding
  host may drive lilmail against a specific external mailbox via a
  shared-secret-gated header seam, with per-request credential isolation.
  Standalone lilmail never trusts client-supplied connection headers unless
  the secret is configured and matches.

**Authentication**
- **OAuth2 / OpenID Connect** with **XOAUTH2** and **OAUTHBEARER** SASL for
  both IMAP and SMTP — authorization-code flow, **PKCE**, automatic
  refresh-token handling — alongside classic password login

**Compose & attachments**
- Plain-text and HTML rich-text compose, drafts (30 s auto-save + IMAP
  restore), recipient autocomplete (recent-recipients + optional CardDAV)
- Multipart attachments and inline `cid:` images (`multipart/related` +
  `multipart/mixed`), outgoing header-injection guard
- Scheduled send (send-later) with encrypted-at-rest transport credentials and
  a bounded-retry drain
- Metadata read from `BODYSTRUCTURE`; content fetched on demand per MIME part
  and streamed via `GET /api/attachment/:id` — the inbox list never buffers
  attachment bytes

**Security**
- Path-safe cache (username sanitized → no path traversal); `0700` dirs /
  `0600` files; atomic cache writes
- SMTP/IMAP/CalDAV/CardDAV dial-time SSRF screen (metadata IPs, loopback,
  RFC1918, IPv6 ULA/link-local rejected before connect)
- Full **Content-Security-Policy** + `SameSite=Lax` session cookie; email HTML
  rendered in a sandboxed `<iframe>` with no scripts; the Edit-Draft compose
  path is defanged separately since it runs in the non-sandboxed app document
- Reading-pane "Display images" opt-in blocks every remote-content vector
  (`<img>`, `<video>`/`<audio>`/`<source>`/`<track>`/`<embed>`/`<iframe>`,
  `<object>`, `<link>`, inline/`<style>` `url()`/`@import`), not just `<img src>`
- **BIMI verified sender brand logo** — DMARC-gated, SSRF-screened, SVG-sanitized,
  fails closed on every ambiguous case
- Dependencies and toolchain tracked against `govulncheck`/Dependabot

**UI/UX — Gmail-inspired, responsive, dark mode**
- Responsive three-pane app shell (resizable, collapsible sidebar / mobile
  drawer, reading pane right/bottom/off)
- Threaded/collapsible conversation list (JWZ algorithm), unread emphasis,
  multi-select + bulk actions, keyboard navigation
- Hand-written CSS design system (Coral & Teal palette, dark mode, no CDN
  dependency)

**AI mail assistant** (opt-in via `[ai].enabled`)
- Compose / summarize / reply suggestions / action-item extraction / phishing
  detection — delegates to a configurable OpenAI-compatible SSE endpoint
- Default endpoint targets the Vulos OS airouter (llmux); fully standalone
  with any compatible provider

**Calendar (CalDAV) & contacts (CardDAV)**
- CalDAV client (discover calendars, list/create/delete events, free/busy)
- End-to-end iTIP/iMIP meeting invites: send `METHOD:REQUEST`, parse a
  received invite, RSVP with `METHOD:REPLY` reflected into the responder's
  own calendar
- CardDAV contact queries feeding recipient autocomplete

**Notifications & real-time** (opt-in via `[notifications].enabled`)
- Server-side **IMAP IDLE** watcher; **SSE** stream → Web Notifications API
- Opt-in native desktop toasts (`gen2brain/beeep`) for local runs
- **Web Push / VAPID** — background push delivery with an auto-generated
  ECDH P-256 key pair, `/api/push/vapid-public` + subscribe/unsubscribe, a
  service worker (`/sw.js`), and a Settings-page toggle

**Multiple accounts**
- Add/switch IMAP+SMTP accounts from Settings; AES-256-GCM encrypted
  credentials; **unified inbox** with concurrent per-account fan-out and
  per-account error isolation

**Vulos OS embed**
- `[server] frame_ancestors` config injects into `Content-Security-Policy:
  frame-ancestors` so the Vulos shell can embed LilMail as an iframe
- The `/v1` API is the standalone contract; lilmail hosts no mail and depends
  on no central Vulos server for its own operation

**Legal / compliance**
- `THIRD-PARTY-NOTICES.txt` (Go stdlib + all vendored modules and browser JS)
  generated from the real dependency graph and served at `/licenses.txt`

**Packaging & distribution**
- **Self-contained binary** — templates and vendored HTMX/Alpine.js are
  embedded via `embed.FS`; runs fully offline with only `config.toml`
- Unit tests (SASL/MIME/attachment-ID/threading/AI/config/security) + **CI**
  workflow
- Semantic-versioned **release pipeline** (GitHub Actions on `v*` tags)

---

## 🔜 Next up

- 🔜 **Nix package + NixOS module** for declarative, reproducible self-hosting.
- 🔜 Client-side paste support for inline `cid:` images in the rich-text
  compose editor (the server-side `multipart/related` plumbing already ships).
- 🔜 IMAP-backed folder move for Archive / Junk in the reading-pane toolbar
  (currently laid out but not yet wired to a backend move).

## 💭 Later / exploratory

- 💭 Local, IMAP-native **filters / rules** and richer server-side flag
  management (the old surface delegated this to a removed central proxy;
  a from-scratch client-side implementation is unscoped).
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
- Shipping its own marketing/landing site (product pages live in vulos-cloud).

Have an idea or want to pick something up? See
[Contributing](README.md#-contributing) and open an issue or PR.
