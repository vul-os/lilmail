# Configuration Reference

lilmail reads `config.toml` from the current working directory at startup. Copy
`config.toml.example` as a starting point:

```bash
cp config.toml.example config.toml
```

All sections except `[server]`, `[imap]`, `[smtp]`, `[cache]`, `[jwt]`, and
`[encryption]` are **optional** and disabled by default.

### How the file is located

The path is the literal string `config.toml`, resolved against the process's
current working directory. There is no `--config` flag, no `$LILMAIL_CONFIG`
environment variable, and no search of `/etc` or `$HOME` — the only command-line
flag lilmail accepts is `-version`, which prints the version and exits. If the
file is missing or malformed the process exits immediately with
`Failed to load config: …`.

Several other paths default to being relative to the working directory too
(`./cache`, `./sessions`, `./accounts.db`, `./vapid.json`), so **the working
directory is part of your configuration**. If you run lilmail from a service
manager, set the working directory explicitly — see
[What lilmail writes to disk](CONFIGURATION.md#what-lilmail-writes-to-disk).

### Config errors that stop startup, and warnings that do not

| Condition | Result |
|-----------|--------|
| `config.toml` missing or not valid TOML | **fatal** |
| `[ai] mode` is neither `remote` nor `embedded` | **fatal** (a typo must not silently fall back to `remote`) |
| `[encryption] key` present but not 16, 24, or 32 bytes | **fatal** |
| `[ssl] enabled = true` with a missing/unloadable cert or key | **fatal** |
| `[ai] mode = "embedded"` that cannot resolve providers or route `model` | **fatal** |
| `[accounts] enabled = true` and the store file cannot be opened | **fatal** |
| `[encryption] key` empty | warning only — login will fail later |
| `[jwt] secret` empty | warning only — tokens are not securely signed |
| `[notifications] webpush = true` and VAPID key init fails | warning only — web push is disabled, the rest keeps running |
| durable KV store cannot be opened — including `backend = "postgres"` with no `postgres_dsn`, an unknown backend name, or an unreachable database | warning only — the server starts, logs `scheduled send unavailable (store open failed)`, and every KV-backed surface reports `501` |

That last row is worth reading twice: a **typo in `[storage]` does not stop the
server**. It logs one line and silently downgrades scheduled send, the vacation
responder, signatures, send-as identities and connected accounts to `501`. If
you configure Postgres, check the log on first start.

---

## `[server]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `3000` | HTTP listen port |
| `username_is_email` | bool | `true` | Send the full email address as the IMAP/SMTP login username |
| `frame_ancestors` | string | `""` | Space-separated CSP `frame-ancestors` origins. Leave empty for same-origin only. Example: `"'self' http://localhost:8080"` |
| `secure_cookies` | bool | `false` | Set the `Secure` flag on the session and CSRF cookies. Enable when serving over HTTPS (direct `[ssl]` or TLS reverse proxy) |

The session cookie is named `session_id`; it is `HttpOnly` and `SameSite=Lax`,
and it holds only an opaque session id — never your mail password. A second,
deliberately JS-readable cookie `_csrf` carries the double-submit CSRF token.

**lilmail does not read `X-Forwarded-For`.** No proxy-header trust is configured,
so the client IP used for rate limiting is the address of whatever connected
directly. Behind a reverse proxy that is the proxy, which means the per-IP login,
send and AI limits become **global** limits rather than per-client ones. If you
need per-client limiting behind a proxy, enforce it in the proxy.

---

## `[auth]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allow_full_email_username` | bool | (inherits `[server] username_is_email`) | What to send as the IMAP/SMTP login username: `true` sends the full address (`alice@example.com`), `false` sends only the local part (`alice`) |

`[auth] allow_full_email_username` and `[server] username_is_email` control the
same thing. `[auth]` is the option going forward; when it is present it wins, and
when it is absent the `[server]` value is used. Most hosted providers want the
full address; some self-hosted Dovecot/Postfix setups authenticate the bare
handle.

---

## `[imap]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `server` | string | — | IMAP hostname |
| `port` | int | `993` | IMAP port |
| `tls` | bool | `true` | `true` dials implicit TLS (imaps, port 993). `false` dials **plaintext IMAP** |

**`tls = false` is not STARTTLS.** lilmail's IMAP path has no STARTTLS upgrade —
setting `tls = false` opens an unencrypted connection and sends your password over
it in the clear. It exists for a plain-IMAP server on a trusted local network (or
a stunnel/sidecar that terminates TLS for you), and for nothing else. STARTTLS is
implemented for **SMTP only** (`[smtp] use_starttls`).

There is no `insecure_skip_verify` for IMAP. A self-signed IMAP certificate will
fail the handshake; only the SMTP client exposes that escape hatch.

**Timeouts.** lilmail allows 10 s to connect, finish the TLS handshake and receive
the server greeting, then 60 s for each IMAP command (an attachment download is
one command). The IDLE watcher is exempt, since IDLE legitimately blocks. Set
`LILMAIL_TIMING=1` in the environment to log how long IMAP connect+login takes,
which tells a slow server from a slow request.

---

## `[smtp]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `server` | string | — | SMTP hostname (derived from IMAP server when omitted: `imap.*` → `smtp.*`) |
| `port` | int | `587` | SMTP port (`587` STARTTLS / `465` implicit TLS) |
| `use_starttls` | bool | `true` | Use STARTTLS upgrade. Set `false` for implicit TLS (port 465) |
| `insecure_skip_verify` | bool | `false` | Skip TLS certificate verification. For self-signed certs only |

SMTP allows 10 s to connect and receive the greeting, then 2 minutes for the rest
of the conversation (authentication, the message itself, QUIT).

---

## `[cache]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `folder` | string | `"./cache"` | Directory for the on-disk email cache **and outbound attachment staging** (`POST /v1/attachments`). Created automatically. If unset/unwritable, composing with attachments returns `503 attachment staging unavailable` while downloads still work |

---

## `[storage]`

Selects the durable key-value backend used for caches and shared state (thread
metadata, recipients, push subscriptions). **Optional** — omit the section to use
the default embedded backend.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `backend` | string | `"bolt"` | `"bolt"` (embedded bbolt, single binary, nothing to run) or `"postgres"` (shared SQL store) |
| `postgres_dsn` | string | `""` | Connection string, required when `backend = "postgres"`. e.g. `postgres://user:pw@host:5432/db?sslmode=require` |

Use `postgres` only when several lilmail/Vulos instances must share one store, or
when another Vulos service needs to read the same data. The Postgres schema
(a single `lilmail_kv` table) is created automatically on first connect.

```toml
[storage]
backend = "bolt"   # default; omit the section entirely for the same effect
# backend = "postgres"
# postgres_dsn = "postgres://lilmail:secret@localhost:5432/lilmail?sslmode=require"
```

### What lilmail writes to disk

Four paths, all relative to the process's working directory unless you set them
to something absolute. This is why the working directory is part of your
configuration.

| Path | Default | Written when |
|------|---------|--------------|
| `./cache` | `[cache] folder` | Always — cached message bodies and metadata |
| `./sessions` | not configurable | Always — server-side session records |
| `accounts.db` | `[accounts] store_file` | Only with `[accounts] enabled = true` — bbolt database of extra accounts |
| `vapid.json` | `[notifications] vapid_key_file` | Only with `[notifications] webpush = true` — the generated VAPID key pair |

Back these up together, or none of them: `accounts.db` holds encrypted
credentials that are useless without the `[encryption] key` from your
`config.toml`, and `vapid.json` is the identity your push subscriptions are
bound to — regenerate it and every existing subscription stops working.

### Shared object storage (`VULOS_STORAGE_BROKER_SECRET`)

lilmail's primary stores are IMAP (the mail) and the KV seam above; it does **not**
keep mail or state in object storage. The only object-storage use is a **supplementary
read-through cache of immutable attachment blobs**, avoiding repeated IMAP pulls of the
same MIME part.

This is **off by default** and is **authenticated**, exactly like the MAIL credential
broker (`LILMAIL_BROKER_SECRET`). It activates only when **all** of these hold:

1. The operator sets the environment variable `VULOS_STORAGE_BROKER_SECRET` to a shared
   secret (set only in deployments behind the Vulos OS gateway). **Setting it is the
   enable signal — there is no separate on/off toggle.** When unset, injected storage
   headers are ignored entirely and lilmail behaves as standalone (IMAP-only).
2. The request presents a matching `X-Vulos-Storage-Broker-Auth` header. It is compared
   against the secret in constant time; an absent or mismatched value means the storage
   headers are ignored entirely (standalone behaviour). This proves the headers came
   from the gateway and were not forged by a client.
3. The Vulos OS gateway injects per-request `X-Vulos-Storage-*` headers
   (`-Endpoint`, `-Bucket`, `-Prefix`, `-Region`, `-Access-Key`, `-Secret-Key`,
   optional `-Session-Token`). An absent/empty `-Endpoint` means "do nothing new".

As an additional SSRF/exfiltration guard, the injected `-Endpoint` must use `https://`
unless it names a loopback or private-network host (e.g. `http://minio:9000`,
`http://127.0.0.1:9000`, an RFC 1918 address, or an internal `*.internal`/`*.local`
name); a plaintext `http://` endpoint to a public host is refused and the request falls
back to IMAP.

Objects are written under `<X-Vulos-Storage-Prefix>/mail/attachments/<id>` (the prefix
is now `<userID>/<appID>/`). A cache miss or any S3 error transparently falls back to
IMAP and is never shown to the user. No config file keys are involved; there is nothing
to set for standalone use.

---

## `[jwt]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `secret` | string | — | Secret for signing JWT session tokens. **Change in production.** Generate with `openssl rand -hex 32` |

---

## `[encryption]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `key` | string | — | AES-GCM key for encrypting credentials/tokens at rest. **Must be exactly 16, 24 or 32 bytes** (AES-128/192/256); any other non-empty length is fatal at startup. 32 is the recommended choice. **Change in production.** |

---

## `[ssl]`

**lilmail does not terminate TLS.** There is one listener and it is plain HTTP
on `[server] port` (`app.Listen` in `main.go`). Enabling this section does not
open a socket on 443, does not redirect HTTP → HTTPS, and does not serve
HTTPS. Terminate TLS in a reverse proxy.

What `[ssl] enabled = true` actually does, and all it does:

1. **Validates the key pair at startup.** `cert_file` and `key_file` must both
   be set and must load as an X.509 pair, or the server exits (`ValidateSSL`).
2. **Emits `Strict-Transport-Security`** — but only when `domain` is also set —
   as `max-age=<hsts_max_age>; includeSubDomains`.

So the section is, in practice, an HSTS switch with a cert-file sanity check in
front of it. Use it when something in front of you *is* terminating TLS and you
want lilmail to send HSTS itself; otherwise set the header in the proxy and
leave this section alone.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Turn on the cert-pair check and (with `domain`) the HSTS header. **Does not enable HTTPS.** |
| `cert_file` | string | — | Path to a TLS certificate (PEM). Required when `enabled`; only loaded to verify it parses |
| `key_file` | string | — | Path to the matching private key (PEM). Required when `enabled`; only loaded to verify it parses |
| `domain` | string | — | Required for HSTS. With `enabled` but no `domain`, no HSTS header is sent |
| `hsts_max_age` | int | `31536000` | HSTS `max-age` in seconds (1 year). Only used when `enabled` **and** `domain` are set |

Those five keys are the whole section. `port`, `http_port` and `auto_redirect`
used to be listed here; they were never read by anything and have been removed
rather than left implying a listener or a redirect that does not exist. If your
`config.toml` still sets them you do not need to do anything — unknown keys are
ignored by the decoder, so the file keeps loading unchanged.

---

## `[oauth2]`

OAuth2/OpenID Connect for authenticating to your IMAP and SMTP server (not a
lilmail user-management system). When enabled, a **Sign in with OAuth2** button
appears on the login page; password login keeps working alongside it.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `client_id` | string | — | OAuth2 client ID |
| `client_secret` | string | `""` | OAuth2 client secret. Leave empty for public PKCE clients |
| `auth_url` | string | — | Authorization endpoint URL |
| `token_url` | string | — | Token endpoint URL |
| `userinfo_url` | string | `""` | UserInfo endpoint (optional — used to resolve the email when omitted from `id_token`) |
| `redirect_url` | string | — | Callback URL. Register this with your provider: `https://yourdomain.com/auth/oauth/callback` |
| `scopes` | []string | — | OAuth2 scopes. Typically `["openid", "email", "profile"]` |
| `mechanism` | string | `"xoauth2"` | SASL mechanism for IMAP/SMTP: `"xoauth2"` or `"oauthbearer"` |
| `email_claim` | string | `"email"` | JWT/UserInfo claim that holds the email address |
| `use_pkce` | bool | `true` | Enable PKCE (S256). Recommended |

---

## `[caldav]`

CalDAV calendar integration. When enabled, month/week calendar views appear and
a Calendar link is shown in the sidebar.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `url` | string | — | CalDAV endpoint or principal URL |
| `auth` | string | `"basic"` | Authentication method: `"basic"` or `"oauth2"` (uses the logged-in user's OAuth2 token) |
| `username` | string | — | Basic-auth username (ignored when `auth = "oauth2"`) |
| `password` | string | — | Basic-auth password (ignored when `auth = "oauth2"`) |

---

## `[carddav]`

CardDAV address-book for recipient autocomplete in the compose modal.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `url` | string | — | CardDAV endpoint URL |
| `username` | string | — | Basic-auth username |
| `password` | string | — | Basic-auth password |

---

## `[notifications]`

Real-time new-mail notifications. All keys are opt-in; setting
`enabled = false` (the default) creates no extra goroutines or routes.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `idle` | bool | `true` | Start an IMAP IDLE watcher per session (recommended; falls back to NOOP poll) |
| `desktop` | bool | `false` | Show native OS toasts via `gen2brain/beeep` (useful for local/desktop runs) |
| `webpush` | bool | `false` | Enable VAPID Web Push for background notifications. Requires HTTPS |
| `vapid_key_file` | string | `"vapid.json"` | Path to the VAPID key-pair JSON file (auto-generated on first start; **protect this file**) |

### Web Push routes (registered only when `webpush = true`)

| Route | Description |
|-------|-------------|
| `GET /api/push/vapid-public` | Returns `{"publicKey":"<base64url>"}` — public, no auth |
| `POST /api/push/subscribe` | Upsert a browser PushSubscription (session auth required) |
| `DELETE /api/push/subscribe` | Remove a subscription by endpoint (session auth required) |

---

## `[ai]`

AI mail assistant. All five AI routes return `404 {"error":"ai_disabled"}` when
`enabled = false`.

LilMail runs **no inference of its own**. `mode` picks where completions happen:

- **`"remote"`** (the default) forwards mail content to a configurable
  **OpenAI-compatible SSE chat-completion endpoint** (just a base URL + Bearer
  token). That endpoint can be a provider directly, the Vulos OS *airouter*
  (`/api/ai/chat`), or the central **llmux** gateway (`/v1/chat/completions`).
  Nothing here is llmux-specific: it is simply a URL you point `endpoint` at.
- **`"embedded"`** links **llmux** (`github.com/vul-os/llmux`) into LilMail as
  an in-process Go library. There is no gateway to deploy and no completion
  hop — llmux does the routing, retries, failover, sovereignty enforcement and
  BYOK inside LilMail's own process, from **llmux's own JSON config**.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch. When false no completion backend is constructed at all, so the feature can emit no packets |
| `mode` | string | `"remote"` | `"remote"` (call an endpoint) or `"embedded"` (run llmux in-process). Any other value is a startup error |
| `endpoint` | string | `"http://localhost:8080/api/ai/chat"` | **remote only.** OpenAI-compatible SSE chat-completion endpoint. Set to llmux's `/v1/chat/completions` to route through the central gateway |
| `api_key` | string | `""` | Static Bearer token sent as `Authorization: Bearer <key>` when no per-request account token is present. For llmux this is typically a standalone virtual key. Leave empty when the endpoint handles auth separately |
| `account_header` | string | `""` | Inbound request header carrying the caller's account token. When set and present, its value is forwarded as `Authorization: Bearer <token>` so a central gateway (llmux) can resolve it to an account and apply BYOK-vs-central + metering — or, in embedded mode, is passed to the in-process gateway's `Authorize`. Falls back to `api_key` when absent. Leave empty for standalone |
| `model` | string | `""` | Model slug forwarded to the endpoint. Empty = endpoint default. **Required in embedded mode**, and must be routable by `llmux_config` |
| `llmux_config` | string | `""` | **embedded only.** Path to llmux's own JSON config (providers, routes, virtual keys, BYOK). Empty = llmux's defaults plus its environment auto-detection (`OLLAMA_HOST`, `OPENAI_API_KEY`, …) |
| `llmux_cache` | bool | `false` | **embedded only.** Opt in to llmux's in-memory response cache. Off by default for privacy — see below |

### Routing through the central llmux gateway (Vulos suite)

In a Vulos suite deployment, point LilMail at llmux so each account's AI usage is
powered per its own choice (BYOK or central) and metered/billed centrally:

```toml
[ai]
enabled        = true
endpoint       = "http://llmux:4000/v1/chat/completions"
account_header = "X-Vulos-Account-Token"   # header the host shell injects per request
# api_key is used only as a fallback when the header is absent
```

LilMail forwards the per-request account token from `account_header` as the
`Authorization: Bearer <token>`. llmux resolves it to an account and decides
**BYOK vs central** and metering on the account's behalf — **LilMail never
decides BYOK/central, it only forwards the token**. See llmux's
`docs/LLM-ACCESS.md` ("Product consumption contract").

For **standalone / BYO** use, leave `account_header` empty and set `endpoint`
straight at a provider (or airouter) with a static `api_key`. With
`enabled = false` (the default) the feature is fully off and no AI routes are
served.

### Embedding llmux in-process (`mode = "embedded"`)

```toml
[ai]
enabled      = true
mode         = "embedded"
model        = "llama3.1"                 # required; must be routable by llmux_config
llmux_config = "/etc/lilmail/llmux.json"  # llmux's own config: providers, routes, keys
llmux_cache  = false
```

LilMail builds the gateway and calls it directly; it never starts llmux's
background work (`Run`/`Start`), so the embedded gateway makes **no outbound
call that a mail action did not cause**. Whatever `llmux_config` says, LilMail
overrides four things, because a mail client must not host them:

| Overridden | Why |
|-----------|-----|
| No listener (`addr`, `socket_path`) | Nothing on this path serves HTTP |
| No price-feed sync (`pricing.sources`, `azure_pricing`) | llmux's defaults are openrouter.ai and a GitHub raw URL. An embedded gateway quietly reaching a price feed from inside a mail client is exactly the surprise embedding is meant to remove. The built-in seed catalog still prices requests offline |
| No `postgres`, no `redis` | Postgres is the one thing llmux connects **eagerly**, and it resolves its DSN from `DATABASE_URL` / `VULOS_DATABASE_URL` — so a shared DSN in the environment would otherwise have LilMail open a database pool for LLM key spend. Cross-replica key/spend state is a reason to run llmux as a service and use `mode = "remote"` |
| No response cache unless `llmux_cache = true` | See below |

Startup fails loudly — rather than 502-ing on the user's first summarize — when
`llmux_config` cannot be read, resolves no providers, or cannot route `model`.

Egress is still governed by llmux's own sovereignty gate: a provider whose base
URL is off-box is **denied** unless that provider sets `allow_egress`.

**Not available embedded: per-account BYOK and the llmux control plane.** llmux
builds neither from its config file — its own composition root wires them — so a
`byok` or `cp` block in `llmux_config` is inert here and every request uses the
central provider keys that config holds. Virtual `keys` *do* work: a key listed
there gates requests through `account_header` exactly as it would over HTTP. A
deployment that needs each account's own keys, or central metering, wants
`mode = "remote"` pointed at a full llmux.

#### Privacy and the embedded cache

Mail content is never written to any persistent store by LilMail, in either
mode. In remote mode it is forwarded to `endpoint` and discarded. In embedded
mode it is handed to the in-process gateway, dispatched to the provider llmux's
config selects, and discarded.

Embedding llmux does bring its **response cache** into LilMail's process, so
LilMail builds the gateway with that cache **disabled by default** — nothing
derived from a message outlives the request, and the default holds even if
`llmux_config` enables the cache itself.

Setting `llmux_cache = true` opts in explicitly. Model responses to your mail
are then held in an in-memory, TTL-bounded, size-bounded LRU keyed by a SHA-256
of the request, until they expire or are evicted. Even then nothing is written
to disk: with `redis` and `postgres` stripped on this path, the network-backed
cache and key stores are unreachable.

### AI routes (registered only when `enabled = true`)

| Route | Description |
|-------|-------------|
| `GET /api/ai/capabilities` | Which of the routes below are usable with the current `[ai]` config |
| `POST /api/ai/compose` | Smart compose / continue / rewrite |
| `POST /api/ai/summarize` | Thread summary + key points + action items |
| `POST /api/ai/reply` | Three reply suggestions (concise / detailed / decline) |
| `POST /api/ai/extract-actions` | Action items with optional due dates |
| `POST /api/ai/phishing` | Phishing / suspicious / clean classification |

---

## `[accounts]`

Multi-account support. When enabled, users can add and switch between additional
IMAP/SMTP accounts from the Settings page.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `store_file` | string | `"accounts.db"` | bbolt database for encrypted additional-account credentials (auto-created) |

### Account routes (registered only when `enabled = true`)

| Route | Description |
|-------|-------------|
| `GET /api/accounts` | List additional accounts (passwords not returned) |
| `POST /api/accounts` | Add an account (validates IMAP credentials; JSON body) |
| `DELETE /api/accounts/:email` | Remove an account |
| `POST /api/accounts/:email/switch` | Switch the active session to this account |

---

## Minimal example

```toml
[server]
port = 3000
username_is_email = true

[imap]
server = "imap.example.com"
port   = 993
tls    = true

[smtp]
server       = "smtp.example.com"
port         = 587
use_starttls = true

[cache]
folder = "./cache"

[jwt]
secret = "change-me-to-a-long-random-string"

[encryption]
key = "a-32-character-encryption-key!!"
```

## Full example

See [`config.toml.example`](../config.toml.example) in the repository root for
a complete annotated example covering all sections.
