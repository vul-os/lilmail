# Configuration Reference

lilmail reads `config.toml` from the current working directory at startup. Copy
`config.toml.example` as a starting point:

```bash
cp config.toml.example config.toml
```

All sections except `[server]`, `[imap]`, `[smtp]`, `[cache]`, `[jwt]`, and
`[encryption]` are **optional** and disabled by default.

---

## `[server]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `3000` | HTTP listen port |
| `username_is_email` | bool | `true` | Send the full email address as the IMAP/SMTP login username |
| `frame_ancestors` | string | `""` | Space-separated CSP `frame-ancestors` origins. Leave empty for same-origin only. Example: `"'self' http://localhost:8080"` |
| `secure_cookies` | bool | `false` | Set the `Secure` flag on session cookies. Enable when serving over HTTPS (direct `[ssl]` or TLS reverse proxy) |

---

## `[imap]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `server` | string | — | IMAP hostname |
| `port` | int | `993` | IMAP port |
| `tls` | bool | `true` | Use implicit TLS (recommended; use `false` for STARTTLS on port 143) |

---

## `[smtp]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `server` | string | — | SMTP hostname (derived from IMAP server when omitted: `imap.*` → `smtp.*`) |
| `port` | int | `587` | SMTP port (`587` STARTTLS / `465` implicit TLS) |
| `use_starttls` | bool | `true` | Use STARTTLS upgrade. Set `false` for implicit TLS (port 465) |
| `insecure_skip_verify` | bool | `false` | Skip TLS certificate verification. For self-signed certs only |

---

## `[cache]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `folder` | string | `"./cache"` | Directory for on-disk email cache. Created automatically |

---

## `[jwt]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `secret` | string | — | Secret for signing JWT session tokens. **Change in production.** Generate with `openssl rand -hex 32` |

---

## `[encryption]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `key` | string | — | 32-byte AES-GCM key for encrypting credentials/tokens at rest. **Must be exactly 32 bytes. Change in production.** |

---

## `[ssl]`

Enable HTTPS termination directly in lilmail (alternative: use a reverse proxy
and set `[server] secure_cookies = true`).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable HTTPS |
| `cert_file` | string | — | Path to TLS certificate (PEM) |
| `key_file` | string | — | Path to TLS private key (PEM) |
| `port` | int | `443` | HTTPS listen port |
| `http_port` | int | `80` | HTTP listen port (for redirect when `auto_redirect = true`) |
| `auto_redirect` | bool | `false` | Redirect HTTP → HTTPS |
| `domain` | string | — | Domain for HSTS header |
| `hsts_max_age` | int | `0` | HSTS `max-age` in seconds. `0` disables HSTS. Recommended: `31536000` (1 year) |

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

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch |
| `endpoint` | string | `"http://localhost:8080/api/ai/chat"` | OpenAI-compatible SSE chat-completion endpoint |
| `api_key` | string | `""` | Bearer token sent as `Authorization: Bearer <key>`. Leave empty when the endpoint handles auth separately |
| `model` | string | `""` | Model slug forwarded to the endpoint. Empty = endpoint default |

### AI routes (registered only when `enabled = true`)

| Route | Description |
|-------|-------------|
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
