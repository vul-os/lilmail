# Changelog

All notable changes to LilMail are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)  
Versioning: [Semantic Versioning](https://semver.org/)

---

## [Unreleased]

### Changed

- **The login page tells an unreachable server from a wrong password.** A
  rejected login is a 401 "Invalid email or password"; a connection failure or
  timeout is a 503 "Could not reach the mail server…"; anything else is a 502.
  Previously every failure read "Invalid credentials or server error".

### Added

- **The mail AI assistant can now run llmux in-process instead of calling a
  gateway.** `[ai] mode = "embedded"` links llmux
  (`github.com/vul-os/llmux`) into LilMail as a Go library: no gateway to
  deploy, no completion hop, and llmux's routing, retries, failover,
  sovereignty enforcement and BYOK all happen inside LilMail's own process from
  llmux's own JSON config (`[ai] llmux_config`). `mode = "remote"` remains the
  default and is unchanged — pointing at a central llmux or the Vulos OS
  airouter is still a first-class deployment, and the `account_header` →
  `Authorization: Bearer` forwarding it exists for is untouched. In embedded
  mode that same account token is passed to the gateway's `Authorize`, which is
  the single auth path llmux's own HTTP shell uses, so an in-process host can't
  get a laxer check than a network client.

  Whatever `llmux_config` says, LilMail overrides four things when it builds the
  embedded gateway: no listener, no price-feed sync (an embedded gateway quietly
  reaching openrouter.ai from inside a mail client is exactly the surprise this
  removes), no Postgres or Redis (Postgres is the one thing llmux connects
  eagerly, and it reads `DATABASE_URL`/`VULOS_DATABASE_URL` — so a shared DSN in
  the environment would otherwise open a database pool for LLM key spend), and
  **no response cache unless `[ai] llmux_cache = true`**.

  That last one keeps a standing promise honest: embedding llmux brings its
  cache into LilMail's process, where it would retain model output derived from
  your mail after the request ended. It is off by default — the default holds
  even when `llmux_config` enables the cache itself — and the opt-in is
  documented for exactly what it retains (an in-memory, TTL- and size-bounded
  LRU keyed by a hash of the request; never written to disk, since `redis` is
  stripped on this path too).

  A misconfigured embedded gateway now fails at startup — unreadable config, no
  providers, or an unroutable `[ai] model` — instead of surfacing as a 502 on
  the user's first summarize.

  With `[ai] enabled = false` (still the default) **no completion backend is
  constructed at all** in either mode. A test injects a recording
  `http.RoundTripper` and asserts zero round trips and no extra goroutines
  across every AI route, with the same recorder proven against both live modes
  so the zero means something.

### Removed

- **`[ssl] port`, `[ssl] http_port` and `[ssl] auto_redirect` are gone.** All
  three were assigned defaults (443, 80, true) and then read by nothing: LilMail
  has one listener, plain HTTP on `[server] port`, so nothing ever listened on
  443, nothing listened on 80, and there was no HTTP → HTTPS redirect to enable.
  Documenting them as "not read" was a stopgap; a config key that names a port
  is a promise, and these three could not keep it.

  **No action is needed on upgrade.** A `config.toml` that still sets them loads
  exactly as before — the TOML decoder reports unrecognised keys through a
  metadata value that `LoadConfig` discards, so unknown keys are ignored rather
  than rejected. That is asserted by a test that runs a config carrying all
  three removed keys through the real load path, so the compatibility claim
  fails loudly if the decoder is ever made strict.

  `[ssl] enabled`, `cert_file`, `key_file`, `domain` and `hsts_max_age` are
  unchanged and still do what they did: validate the certificate pair at startup
  and emit `Strict-Transport-Security`.

---

## [1.14.0] - 2026-08-06

### Added

- **Client-side JavaScript is now type-checked and linted, and CI fails the
  build if it isn't clean.** `assets/js/*.js` and `assets/sw.js` — the code
  that handles Web Push subscriptions and the session bearer token — had sat
  behind no linter and no type checker at all; `go build`/`go vet` strip types
  without checking this code either. JSDoc annotations plus `tsc` (a browser
  project via `tsconfig.json` and a separate service-worker project via
  `tsconfig.sw.json`, since a service worker has no DOM and needs its own
  global-scope types) and a type-aware ESLint flat config now cover both, and
  a new `client-js` CI job runs both with no `continue-on-error`. Two of the
  three bugs described under Fixed below were caught this way, by the type
  checker, not by manual review.
- **Signed, checksummed releases — and a verifier that fails closed.** Nothing
  in the release pipeline previously vouched for the bytes it published. The
  release workflow now stages every asset into `release/`, emits a `SHA256SUMS`
  manifest **over that directory** (so "published" and "covered" are the same
  set by construction, not two hand-maintained lists), asserts that the manifest
  has exactly one line per staged asset, and attaches a sigstore build-provenance
  attestation minted from the workflow's OIDC identity — no long-lived signing
  key exists, so there is none to leak, own or rotate. A release that staged
  nothing, or whose manifest does not cover what it staged, is now a **red**
  release rather than a green one with an empty manifest.
  `scripts/verify.sh` is the user-facing half: it fetches the manifest, looks up
  the **exact** entry for the requested asset (string comparison on field 2 —
  a substring/regex match would let `…tar.gz.sig` answer for `…tar.gz`) and
  compares digests. There are two outcomes, verified or non-zero with a distinct
  diagnostic; there is no `--skip-verify` and **no path where an absent
  `SHA256SUMS` means "nothing to check"** — that shrug is the bug the file
  exists not to have, because it converts *"I don't know"* into *"it's fine"*.
  The release job runs `verify.sh` against its own output before publishing, so
  producer and consumer cannot drift apart silently.
- **`make verify-selftest`** (also a CI job, and a release-job step) — 24
  synthetic-origin cases covering every refusal: manifest 404, manifest served
  as an HTML error page (both by content-type and by sniffing a lying one),
  empty/junk/truncated manifest, no entry for the asset, the `.sig` and
  regex-wildcard name traps (one arranged so a naive substring match would
  report **exit 0 on an artifact nobody vouched for**), asset 404, asset served
  as HTML, truncated download, digest mismatch, plaintext origin, missing curl
  or digest tool, and `--attest` with no `gh` installed. Each case asserts the
  exit code **and** that a diagnostic was printed — a guard that aborts silently
  reads as a crash, not a refusal, and "died at a pipeline under `set -e`" is
  precisely how a sibling installer's unreachable guard shipped.

- **`docs/SIGNING.md`** — the exact wire format of every non-session request
  authentication lilmail performs: the mail-broker and storage-broker header
  seams (shared bearer secrets, constant-time compared — with an explicit list of
  the properties they do *not* have: no HMAC, no timestamp window, no nonce, no
  replay protection, no body binding) and the AWS SigV4 object-storage signer
  (canonical URI, signed-header set, canonical request, string-to-sign, key
  derivation, `Authorization` layout, timestamp/nonce/retry semantics). It states
  lilmail's standing position that it emits **no outbound webhooks** and has no
  bespoke webhook HMAC, and points clients at SSE / Web Push / polling instead.
- **Known-answer vectors for the SigV4 signer**
  (`storage/sigv4_vectors_test.go`). Two published vectors — GET with an empty
  payload, and PUT with a body, a session token and a percent-encoded key — pin
  the canonical URI, payload hash, `SignedHeaders` list, credential scope and
  signature byte for byte. The expected values were derived from the prose in
  `docs/SIGNING.md` by a separate implementation, so the document and the code are
  held to each other rather than the test merely echoing the code. A companion
  test proves the signature actually binds the secret key, region, timestamp,
  payload, key, bucket, method and session token, and a coverage test stops the
  vector corpus silently shrinking.
- **Documentation gates for the `/v1` contract**
  (`handlers/jsonapi/api_docs_test.go`). Every route `Register()` mounts must
  appear in `docs/API.md` **with its method**, every `/v1` path the document
  advertises must actually be mounted, and a floor assertion plus a
  named-route list keep the walk from under-running (it enumerates the widest
  surface — CalDAV, CardDAV and the broker seam all enabled — so the
  conditionally-mounted groups are covered too).

### Changed

- **The client JavaScript inlined in `templates/layouts/main.html` is now in
  ordinary static files.** All five inline `<script>` blocks — VAPID push
  subscribe/unsubscribe, SSE/desktop-notification wiring, and the Alpine/HTMX
  shell (store setup, CSRF header injection, keyboard navigation) — moved to
  `assets/js/push.js`, `assets/js/notifications.js`, and `assets/js/app.js`;
  the template now only carries `<script src="...">` tags. Behavior is
  unchanged. One block had read the session bearer token via server-side
  template interpolation (`const token = '{{.Token}}'`), which cannot exist in
  a static file; it now reads the same value from the `#app-token` element's
  `data-token` attribute that the template already renders alongside it —
  same value, same empty-string fallback when there's no token.
- README's mermaid diagram had text at roughly 2.9:1 contrast on a white
  background — under WCAG AA — because its old theme used a transparent node
  fill with grey text and grey edges, so legibility depended entirely on
  GitHub's light/dark background. Every node now has an explicit opaque fill
  with light text (three tiers — entry, server, backend), so contrast holds on
  both color schemes. `docs/ARCHITECTURE.md`'s request-lifecycle diagram had no
  theme at all (bare mermaid defaults) and inherited the same problem; it now
  uses the same palette so both diagrams read consistently, and its published
  site mirror (`site/docs/architecture.md`) was regenerated to match. Separately,
  README's `Server` node label was wrapping "JSON API" across two lines
  mid-term because a line was left for mermaid's automatic word-wrap to break
  wherever it fell; line breaks in that label are now explicit so it wraps at
  word/phrase boundaries instead. No diagram content or wording changed in
  either file.
- **CI now runs the checks the repo already claimed.** Added `gofmt`, switched
  the test step to `go test -race` (the broker seam copies out of a pooled
  request buffer and the unified inbox fans out per account — exactly what a
  non-race run cannot police), and wired in the published-docs gate, which
  `site/README.md` had advertised as the CI gate since it was written but which
  nothing ran. `make check` now mirrors CI step for step.
- **The published docs are generated, not copied, and the gate checks links.**
  `scripts/sync-site-docs.mjs` compared source bytes to copy bytes and reported
  `site/docs is in sync` — which it was. What it could not see is that
  `site/docs.html` serves a bundle of eight markdown files, so ten links that
  are perfectly valid inside the repo were dead inside it: `../config.toml.example`,
  `TASKS.md`, `README.md#contributing` and `docs/screenshots/README.md` resolved
  against `site/` and 404'd; four `[1.10.0](#1100---2026-06-22)` changelog
  cross-references and one `[Attachments](#attachments)` were bare fragments,
  which the viewer's hash router reads as a *document slug* and so navigated the
  reader to a different chapter entirely; and
  `CONFIGURATION.md#shared-object-storage-vulos_storage_broker_secret` used
  GitHub's anchor where `docs.html`'s `slugify()` deletes underscores and
  collapses hyphen runs. `site/gen` now generates `site/docs` and rewrites each
  link against what the site really serves, and `site/gen/gen_test.go` fails on
  drift **and** on any link or anchor that would not resolve — including one the
  generator sent to an absolute repo URL that names a path this repo does not
  contain. It also pins the five `docs.html` behaviours the rewriting depends on
  and the viewer's `slugify()`, so changing the viewer cannot quietly invalidate
  the gate. Source documents keep their GitHub-correct links; the rewriting
  happens only in the published copy.
- **`docs/API.md` documents the whole contacts surface.** Nine mounted routes
  were absent from the reference — contact groups (list/create/rename/delete),
  vCard/CSV import + export, photo upload/delete, and frequently-contacted — as
  was the `?group=` filter on `/v1/contacts/cards`. The `Contact` shape was
  documented as seven fields when it carries twenty-two (structured name,
  TYPE-labelled emails/phones/addresses/websites/IMs, birthday, anniversary,
  department, nickname, file-as, groups, photo, starred), and the `Email` shape
  omitted `toNames`, `cc`, `invite`, `unsubscribe` and `brand`. All now
  specified, with the bounds, sanitisation rules and degradation behaviour a
  client needs.
- **The auth model is stated up front.** `docs/API.md` now opens with the fact
  that lilmail has no account system at all — no sign-up, no user table, no
  tenant, no credential of its own — and that the only two ways to authenticate
  a request both resolve to one connection to the user's own mailbox.

### Removed

- **Five unreferenced embedded assets** — `apple-touch-icon.png`, `icon-48.png`,
  `lilmail-logo.svg`, `lilmail.png`, `lilmail.svg`. `//go:embed all:assets`
  compiles everything under `assets/` into the binary, and none of these were
  reachable from any template, stylesheet, service worker, manifest or Go source
  (the templates use `icon-16/32/180`, `icon.png`, `lilmail-favicon.svg` and
  `og-image.png`; the manifest uses `icon-192`/`icon-512`). `assets_embed_test.go`
  now fails on any asset that is embedded but referenced by nothing, so the
  duplicates cannot silently reaccumulate. The vendored `*.LICENSE` files are
  allow-listed with their reason — they must travel with the bundles.
- **`docs/assets/lilmail-logo.png`** — a 2720×880 render of the current coral
  mark that a brand-verification sweep found had no reference anywhere in the
  tree (README, docs, `site/`, templates, or Go source); grepped for
  `lilmail-logo.png` repo-wide with zero hits before removal. Unlike the two
  `.svg` files removed above, this one already carried the correct, current
  mark — it just wasn't linked from anywhere, so it sat as dead weight in
  `docs/assets/`. Not embedded in the binary (`docs/` is outside
  `//go:embed all:assets`), so `assets_embed_test.go` could not have caught
  it; this was a manual, independent-verification find.
- **`docs/demo.png`** and **`site/assets/lilmail-wordmark.png`** — a repo-wide
  orphan sweep found both had zero references anywhere in the tree (README,
  `docs/`, `site/`, templates, Go source, `site/gen`), confirmed by grepping
  each basename repo-wide (`grep -rn demo.png .` / `grep -rn wordmark .`) with
  no hits beyond the file's own path. `docs/demo.png` was a 1846×963 pre-rebrand
  screenshot (May 23) superseded by `docs/screenshots/hero.png`; the README's
  hero image and gallery both use the `docs/screenshots/*.png` set only.
  `site/assets/lilmail-wordmark.png` was a byte-identical duplicate of
  `docs/assets/lilmail-wordmark.png` — the site's own pages (`index.html`,
  `docs.html`) use `lilmail-favicon.svg` plus a live-text `<i>lil</i>mail`
  wordmark, never the PNG. Neither is embedded in the binary or part of the
  `site/gen` published bundle, so no existing gate could have caught either;
  this was a manual, independent-verification find.

### Fixed

- **Push notifications could silently stop working after a browser-triggered
  resubscribe, with no error surfaced anywhere.** The service worker's
  `pushsubscriptionchange` handler read `event.oldSubscription.options`
  unconditionally, but `oldSubscription` is nullable per spec — a browser is
  not always able to supply the prior subscription. When it was null, the
  handler threw before `event.waitUntil()` ran, silently dropping the
  browser's automatic-resubscribe attempt; push delivery would simply stop
  until the user happened to notice and re-enabled it by hand in Settings. Now
  falls back to a fresh VAPID subscribe when there's no prior subscription to
  reuse.
- **A `ReferenceError` could break live-mail notifications entirely in
  browsers without the Notification API.** The handler that raises a desktop
  notification for each new-mail SSE event referenced `Notification.permission`
  on every message regardless of whether the Notification API existed in the
  browser, throwing on the first message received in that case. Now guarded on
  a captured support flag.
- Corrected a code comment that misstated why `/api/push/*` is exempt from
  CSRF checks: it claimed the routes were safe because they carry
  `Authorization: Bearer` headers, but the handlers never read that header —
  `CookieSameSite: "Lax"` is what actually protects them, the same as every
  other protected route. No behavior change; the routes were not exploitable
  either before or after this correction, only the comment was wrong.
- `global.d.ts` (ambient client-JS types) and a tsconfig for the service
  worker had briefly been added inside `assets/`, which
  `//go:embed all:assets` compiles into the release binary — both would have
  shipped and been publicly servable at a URL under `/assets/`. Moved to
  `types/global.d.ts` and a repo-root `tsconfig.sw.json`; no content change.
- **The app-icon set was rendering as a blank coral square.** `icon.png`,
  `icon-16.png`, `icon-32.png`, `icon-180.png`, `icon-192.png` and
  `icon-512.png` — the PWA install icon, `apple-touch-icon`, favicon PNG
  fallback and desktop-notification icon — had shipped since the coral
  rebrand (`3b386d5`) missing the white envelope-flap glyph entirely, so a
  home-screen install or a system notification showed an unmarked orange
  tile indistinguishable from any other app. Re-rendered all six directly
  from `brand/logo.svg`, the approved mark, at their existing pixel sizes.
  `assets/lilmail-favicon.svg` and `site/assets/lilmail-favicon.svg` also
  carried a 2px drift in the flap's vertical position from a hand
  re-creation instead of a copy; both are now byte-identical to
  `brand/logo.svg`. Also removed `docs/assets/lilmail-logo.svg` and
  `docs/assets/lilmail-mark.svg`, two unreferenced leftover brand files
  depicting an earlier, superseded indigo/parchment envelope design that
  contradicted the shipped coral mark.
- Corrected a garbled sentence in the `/v1` snooze description.
- The 1.13.0 send-as-identities entry described pushing aliases to a central
  engine's `/internal/identities`, which the same release removed and which no
  code in this repository has ever called. Rewritten to describe what actually
  ships: local validation, KV storage keyed by the authenticated mailbox, and
  the user's own SMTP server remaining the send-as authority.
- Retired the last "CP" / "control plane" naming for the embedding host from
  code comments and the changelog, finishing the rename the surrounding commits
  began, and fixed a duplicated phrase in the 1.13.0 removal notes.
- `ROADMAP.md` listed "shipping its own marketing/landing site" as a non-goal
  while `site/` shipped exactly that; the roadmap now describes the static
  bundle accurately (and notes it is not embedded in or served by the binary).
  Its Contributing link also pointed at a stale `#-contributing` anchor.
- The VAPID key-management doc comment named a `GenerateAndSaveVAPIDKeys`
  function that does not exist (`LoadOrGenerateVAPIDKeys` does).

## [1.13.0] - 2026-07-17

### Added

- **BIMI verified sender brand logo.** On the single-message read, when the
  message passed DMARC (implying SPF-or-DKIM alignment with the `From` domain,
  reused from the already-parsed `Authentication-Results` — no
  re-authentication), lilmail looks up `default._bimi.<domain>`, fetches the
  `l=` logo, sanitizes it, and attaches it as `models.Email.Brand` for the
  reading pane to render. Every gate fails closed: no DMARC pass → no lookup;
  no/invalid BIMI record → no logo; a non-`https` `l=` is never fetched; the
  fetch is SSRF-screened (`screenDialIP`, no private/loopback/metadata, no
  redirects, size-capped, and regression-tested against the real dial screen)
  so a sender-controlled URL can't probe the server's network; any
  `<script>`/`<foreignObject>`/event-handler/external-reference in the SVG
  voids the whole logo. Resolution is per-domain and cached server-side (never
  per-recipient), so a sender can't use it as a tracking beacon, and never
  blocks a message open.
- **Send-as identities write path** — `PUT /v1/settings/identities` (the
  surface was previously read-only, so the compose "From" selector could only
  ever show the primary address and `POST /v1/messages` ignored `from`
  entirely). Aliases are validated locally (CR/LF/NUL rejected, then address
  shape) and stored in the durable KV keyed by the authenticated mailbox, so a
  caller only ever edits their own list. Compose, draft-save, and scheduled
  send all gate `From` on the primary address or a registered identity (else
  `403`); a scheduled send's ownership key stays the authenticated mailbox
  even when `From` is an alias. lilmail is **not** the send-as authority: the
  user's own SMTP server re-checks the `From` at submission time and remains
  the thing that can refuse it. Send-as only — an alias does not become an
  inbound address. Documented in `docs/API.md`.
- **Third-party licence notices.** lilmail redistributes other people's code
  (51 Go modules plus vendored htmx/Alpine.js) but shipped none of the
  required attribution — vendored bundles had their licence headers stripped
  by the minifier and nothing was surfaced to users. `THIRD-PARTY-NOTICES.txt`
  (generated from the real module graph by `scripts/gen-notices.sh`, now also
  covering the Go standard library's BSD-3 licence + patent grant) is embedded
  in the binary and served at `/licenses.txt`, linked from the login page and
  Settings → About; upstream licence files sit next to each vendored bundle.
  The long-dead, no-longer-loaded `assets/vendor/tailwind.js` was removed
  rather than attributed.

### Changed

- **Reframed as an independent PIM client.** lilmail is a standalone mail +
  calendar + contacts client that talks to the user's **own**
  IMAP/SMTP/CalDAV/CardDAV account and exposes a stable `/v1` JSON API. It
  hosts no mail and depends on no central Vulos server; the Vulos OS
  integrates it over `/v1`. README, ROADMAP, ARCHITECTURE, and API docs were
  reworked to this model across several passes: dropping the stale "Vulos
  Mail product" / central-coupling framing from docs, code comments, and the
  bundled AI-assistant prompts; correcting the "Part of VulOS" product map
  (OS, Office, Files, Relay, llmux — Talk/Meet are archived); making the
  README self-contained; and fixing the GitHub org in badges/links
  (`exolutionza` → `vul-os`, matching the actual remote).

### Removed

- **Dropped the central mail-engine feature-proxy coupling from `/v1`.**
  Several `/v1` surfaces existed only to reverse-proxy to a central mail
  engine's `/internal/*` endpoints and were permanent `501`s in
  every standalone deployment. Removed the six proxies — **rules/filters,
  threads, categories, smart-folders, team-inbox, spam-settings** — and the
  best-effort vacation/identities/snooze push-to-central paths, keeping the
  local KV read-model + IMAP behaviour. The header credential-injection seam
  remains but is now a generic per-request credential injector (not a "Vulos
  Cloud CP" custody path). The standalone `/v1` contract (mail +
  `/v1/calendar` + `/v1/contacts`) is unchanged; the removed routes are locked
  out by a regression guard (~5,000 LOC removed).
- **Dead central-engine remnants.** `vacationActive`/`shouldAutoReply` (an
  out-of-office responder is delivery-path logic owned by the account's own
  mail server, not lilmail) and the orphaned `ThreadID`/`Category`/
  `SmartFolder`/`SmartFields` fields on `models.Email` — server-side
  classification written only by the deleted `/v1` augmentation, always
  absent (`omitempty`) from standalone output — were deleted along with a
  scattering of other provably-unused non-exported declarations
  (unreferenced regexes/maps/fields flagged by `staticcheck U1000`). No
  exported symbol or behaviour change.
- **HTML marketing landing site.** Product landing pages are now centralized
  on vulos.org; lilmail no longer embeds or serves its own (`/site/*` mount
  and embedded `siteFS` removed). `/` now redirects a logged-out visitor
  straight to `/login` (a signed-in user still lands on `/inbox`).

### Fixed

- **Single-part `text/html` messages rendered as raw source, and quoted-
  printable bodies were left undecoded (#9).** Message parsing never reversed
  `Content-Transfer-Encoding` and only inspected the top level of the MIME
  tree, so a single-part `text/html` message (e.g. Anthropic's "Secure link to
  log in to Claude.ai") fell into the non-multipart branch, was assigned to
  `Email.Body` instead of `Email.HTML`, and showed its HTML source instead of
  its content, with `=3D`/soft line breaks throughout. Replaced with a
  recursive `collectBodies()` that routes leaves by media type,
  transfer-decodes base64/quoted-printable, recurses through nested
  containers (`multipart/mixed` › `multipart/alternative` previously matched
  neither `text/plain` nor `text/html` and rendered blank), and skips
  `Content-Disposition: attachment` leaves. The message-list preview had the
  matching problem — it now transfer-decodes first, skips `<style>`/`<script>`
  and comments, resolves entities, and drops zero-width preheader padding
  instead of showing raw CSS resets or base64 gibberish; the preview window
  grows 512 → 4096 bytes.

### Security

- **CRITICAL: stored XSS via Edit Draft.** The Edit-Draft path stashed the
  raw, unsanitized HTML of a viewed message into `data-html`; `restoreDraft()`
  then assigned it via `innerHTML` into the compose editor in the main
  (non-sandboxed) app document, where `<img src=x onerror=...>` /
  `<svg onload=...>` execute under the CSP's `'unsafe-inline'`. Any
  attacker-controlled HTML mail filed into a folder whose name contains
  "draft" therefore yielded stored XSS in lilmail's own origin on Edit Draft.
  Fixed server-side: the HTML feeding the Edit-Draft slot is now defanged by
  a strict allowlist/denylist policy (strips `script`/`style`/`iframe`/`svg`/
  `object`/`embed`/`form`, all `on*` handlers, and `javascript:`/`vbscript:`/
  `data:` URLs), extracted into a shared `handlers/htmlsafe` package. The
  sandboxed reading-pane iframe path is untouched.
- **Reading-pane "Display images" opt-in only blocked `<img>`, letting other
  remote-content vectors auto-load before consent** (tracking-pixel /
  privacy bypass, not XSS — the frame has no `allow-scripts`).
  `blockRemoteContent` is now tag-aware and neutralises every remote
  (`http`/`https`/protocol-relative) fetch vector: `<img>`/`<image>` (the old
  regex never matched `<image>`), `<input type=image>`, `<video>` poster,
  `src` on `<video>`/`<audio>`/`<source>`/`<track>`/`<embed>`/`<iframe>`,
  `<object data>`, `<link href>`, and remote `url()`/`@import` in inline
  `style=` **and** `<style>` blocks. `data:`, `cid:`, and relative references
  are left untouched.
- Bumped `golang.org/x/crypto` 0.45.0 → 0.52.0, clearing 13 Dependabot
  advisories (7 critical / 2 high / 4 moderate) in `x/crypto/ssh` and related
  packages pulled in transitively; `govulncheck` reports 0 reachable
  vulnerabilities after the bump. Pulls forward `x/sync`, `x/sys`, `x/text`
  as required.
- Pinned the build toolchain to go1.25.12, clearing the one *reachable*
  `govulncheck` finding (`GO-2026-5856`, a `crypto/tls` ECH privacy leak
  reached via the S3 and SMTP TLS paths); Dependabot's other 14 alerts are in
  unreachable modules.

---

## [1.12.1] - 2026-07-10

### Fixed

- **Plain IMAP (`tls = false`) was ignored — connections always used TLS (#8).**
  The `[imap] tls` field was shown in `config.toml.example` but was absent from
  `IMAPConfig`, so it was never parsed; every connection used implicit TLS and a
  plain-IMAP server failed with `tls: first record does not look like a TLS
  handshake`. `IMAPConfig` now has a `tls` field (defaults to `true`, so existing
  TLS setups are unchanged); `tls = false` dials plain IMAP. Adds
  `api.NewClientTLS` + a config regression test.
  (`config/config.go`, `handlers/api/client.go`, `handlers/web/auth.go`)
- **Brokered spec buffer-aliasing (per-account routing corruption).** In
  brokered mode the per-request `brokerSpec` was built from `c.Get(...)`
  strings that alias fasthttp's recycled request buffer; because the spec is
  stored in `c.Locals` and its fields (e.g. the CardDAV URL) are used as
  per-account cache/map keys, a later pooled request could silently overwrite an
  earlier request's retained spec — surfacing as an intermittent cross-account
  contact bleed. Every header value is now copied (`strings.Clone`) as the spec
  is parsed, so a spec owns its own memory. (`handlers/jsonapi/broker.go`)

### Tests / Docs

- De-flaked the scheduled-send tests: a `now+1s` deadline formatted at RFC3339
  **second precision** truncates sub-second and could land ~1s in the past,
  firing before the "not sent yet" assertions — scheduled 2s out instead, and
  widened the wall-clock-gated drain waits so `-race` + full-suite CPU contention
  can't beat the poll cadence.
- README: documented lilmail's role in the Vulos **cell / edge model** — the
  `/v1` JSON mail-API library each cell's mailbox serves, behind a minimal
  central forwarding relay, with `@vulos.to` as the central login.

---

## [1.12.0] - 2026-07-06

### Added

- **Scheduled send (send-later) over `/v1`** — `POST /v1/messages` with a future
  RFC3339 `sendAt` persists the compose payload instead of sending now (`202
  Accepted { scheduled, id, sendAt }`), and a durable, poll-based drain delivers
  it at the due time. A new `/v1/scheduled` surface lists (`GET`), cancels
  (`DELETE /v1/scheduled/:id`), and edits time/body (`PATCH /v1/scheduled/:id`) of
  pending sends, scoped to the authenticated account (another account's id is
  `404`, no cross-account leak). The SMTP transport (host/port/secret) is captured
  at schedule time and **encrypted at rest**; the drain is at-least-once (delete
  only after a successful send) with a bounded retry budget so a permanently
  failing send cannot loop or pin a credential forever. OAuth (short-lived token)
  accounts get a tighter 12 h horizon; password accounts get up to 1 year. Every
  fire rebuilds the MIME through the **same** `BuildMIMEMessage` engine, so the
  header-injection guard and `cid:` handling run at actual send time. Enabled by
  wiring a durable KV store (`NewWithStore`); an unconfigured build reports `501`
  rather than silently dropping mail. See `docs/API.md` → Scheduled send.
- **Inline `cid:` images in outgoing mail** — `api.OutgoingAttachment` gains
  `ContentID` + `Inline`, and `BuildMIMEMessage` now wraps the HTML body and
  inline parts in a `multipart/related` container (HTML root + `Content-ID: <id>`,
  `Content-Disposition: inline` parts), nesting regular attachments in the outer
  `multipart/mixed` — i.e. `mixed( related( alternative(text, html), inline… ),
  attachments… )`. The `/v1/messages` and `/v1/drafts` attachment refs accept
  `{"inline":true,"contentId":"…"}` (orthogonal to the `token`/`data` byte source),
  so a client can reference `<img src="cid:…">` instead of shipping fat
  `data:` URIs. Content-IDs are validated against header injection. No-inline
  messages are byte-for-byte unchanged. The client-side paste switch is a
  follow-up. See `docs/API.md` → Attachments.
- **iTIP/iMIP meeting invites end-to-end** — send a calendar invite
  (`METHOD:REQUEST` iCalendar part on the outgoing mail), parse a received invite
  from an inbound message (`Email.Invite` with attendees + the recipient's own
  `MyPartStat`), and reply with an RSVP via `POST /v1/calendar/rsvp`
  (`METHOD:REPLY`, and the event is reflected into the responder's own calendar).

### Security

- **Header-injection guard on the outgoing MIME path** — `BuildMIMEMessage` and
  `SMTPClient.SendMail` now reject bare CR/LF/NUL smuggled into the `To`/`Cc`/
  `From`/`Subject`/threading headers (via `validateHeaderValue`), closing a
  silent-Bcc / message-split vector found by the wave-49 tests. `cid:` Content-IDs
  are validated against a strict token shape. No-injection messages are unchanged.
- **Transport-layer SSRF + traversal coverage (wave-49)** — a coverage-driven
  security pass on the IMAP/SMTP/DAV transport primitives: the dial-time IP screen
  (`screenDialIP`) and rebind guard refuse metadata IPs, loopback, RFC1918, IPv6
  ULA/link-local/unspecified, and metadata-host-by-name for CalDAV/CardDAV URLs;
  attachment part-path parsing (`parsePartPath`, `DecodeAttachmentID`) fails closed
  on non-numeric, traversal-shaped, and truncated input; malformed MIME decoding
  no longer panics.

---

## [1.11.0] - 2026-06-28

### Added

- **Brokered calendar & contacts for `/v1`** — extends the brokered credential
  mode to `/v1/calendar/*` and `/v1/contacts`. When the embedding host sends
  the new
  `X-Vulos-Mail-Caldav-Url` / `X-Vulos-Mail-Carddav-Url` headers (only honored
  behind the same valid-broker-secret gate), lilmail builds the CalDAV/CardDAV
  client **directly from those per-account URLs**, authenticating with the
  `X-Vulos-Mail-Secret` access token as an HTTP `Authorization: Bearer` header —
  reusing the existing oauth2/bearer mode in `handlers/api` (`NewCalDAVClient`,
  new `CardDAVContactsBearer`), no new DAV library. When a brokered request omits
  the relevant DAV URL, the read routes return an empty result and the
  create/delete-event routes return `501 Not Implemented` ("not available for this
  account") **without touching the session**. The calendar/contacts routes are now
  registered when CalDAV/CardDAV is enabled **or** the broker path is active, so
  they exist in CP deployments. **Security:** same gate as the mail routes — if
  `LILMAIL_BROKER_SECRET` is unset or the secret mismatches, the DAV URL headers
  are ignored entirely. Standalone/session behaviour is unchanged. Outlook/
  Microsoft Graph calendars are not covered (CalDAV/CardDAV only).
- **Brokered credential mode for `/v1`** — lets an embedding host
  reverse-proxy to lilmail and drive it against a per-request **external**
  mailbox (Gmail / Outlook / IMAP) whose credentials that host custodies. When a
  `/v1` request presents a valid broker secret (`X-Vulos-Broker-Auth` matched
  against the new `LILMAIL_BROKER_SECRET` env var via a constant-time compare),
  lilmail builds the IMAP/SMTP client **directly** from the injected
  `X-Vulos-Mail-*` headers (`xoauth2` access token or `plain` password) instead
  of the session→`CreateIMAPClient` path. Wired through the mail routes
  (folders, messages, single message, search, flags, delete, compose, drafts);
  calendar/contacts remain session/CalDAV-gated. **Security:** if
  `LILMAIL_BROKER_SECRET` is unset or the secret mismatches, the brokered headers
  are ignored entirely and the request falls back to normal session auth, so
  standalone lilmail never trusts arbitrary client-supplied connection headers.
  New `handlers/jsonapi/broker.go`; documented in [docs/API.md](docs/API.md).
- **JSON API (`/v1`)** — a clean JSON/REST surface served alongside the HTMX UI,
  for rich clients and scripting. Endpoints:
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

## [1.10.0] - 2026-06-22

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

## 1.9.0 - 2026-06-16

> Documented ahead of release and shipped as part of the [1.10.0](#1100---2026-06-22)
> tag; there is no separate `v1.9.0` git tag.

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

## 1.8.0 - 2026-06-15

> Documented ahead of release and shipped as part of the [1.10.0](#1100---2026-06-22)
> tag; there is no separate `v1.8.0` git tag.

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

## 1.7.0 - 2026-06-15

> Documented ahead of release and shipped as part of the [1.10.0](#1100---2026-06-22)
> tag; there is no separate `v1.7.0` git tag.

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

## 1.6.0 - 2026-06-01

> Documented ahead of release and shipped as part of the [1.10.0](#1100---2026-06-22)
> tag; there is no separate `v1.6.0` git tag.

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

- `config.toml` renamed to `config.toml.example` (added to `.gitignore`) so
  placeholder secrets are never committed.  Copy it to `config.toml` to run.
- `strings.Title` (deprecated) replaced by a local `titleCase` helper.
- `io/ioutil` (deprecated) replaced with `io`/`os` equivalents throughout.
- `SMTPClient` constructors take an explicit `useStartTLS bool` parameter
  (previously always STARTTLS).
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

---

## [1.4.0] - 2026-05-24

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

---

[Unreleased]: https://github.com/vul-os/lilmail/compare/v1.14.0...HEAD
[1.14.0]: https://github.com/vul-os/lilmail/compare/v1.13.0...v1.14.0
[1.13.0]: https://github.com/vul-os/lilmail/compare/v1.12.1...v1.13.0
[1.12.1]: https://github.com/vul-os/lilmail/compare/v1.12.0...v1.12.1
[1.12.0]: https://github.com/vul-os/lilmail/compare/v1.11.0...v1.12.0
[1.11.0]: https://github.com/vul-os/lilmail/compare/v1.10.0...v1.11.0
[1.10.0]: https://github.com/vul-os/lilmail/compare/v1.4.0...v1.10.0
[1.4.0]: https://github.com/vul-os/lilmail/releases/tag/v1.4.0
[1.0.7]: https://github.com/vul-os/lilmail/releases/tag/v1.0.7

Note: `v1.6.0`, `v1.7.0`, `v1.8.0`, and `v1.9.0` were documented as part of the
work that shipped in the `v1.10.0` release, but were never cut as their own git
tags — there is no `v1.6.0`…`v1.9.0` ref to link to on GitHub.
