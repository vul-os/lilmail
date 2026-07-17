# Getting Started with lilmail

lilmail is a single Go binary — there is no build step for the frontend and no
database to provision. This guide covers installation, first-run, and the most
common configuration scenarios.

## Prerequisites

| Requirement | Notes |
|-------------|-------|
| Go 1.23+ | Only needed to build from source |
| IMAP server | Any IMAP4rev1 server (port 993 TLS or 143 STARTTLS) |
| SMTP server | Any SMTP server (port 587 STARTTLS or 465 implicit TLS) |

A pre-built binary is available on the
[Releases](https://github.com/vul-os/lilmail/releases) page — no Go
installation required to run it.

## Installation

### Option A — pre-built binary

1. Download the archive for your OS from
   [Releases](https://github.com/vul-os/lilmail/releases).
2. Extract the archive to a directory of your choice.
3. Copy `config.toml.example` to `config.toml` in the same directory (or use
   the example below).
4. Edit `config.toml` with your mail server details.
5. Run the binary: `./lilmail`

### Option B — build from source

```bash
git clone https://github.com/vul-os/lilmail.git
cd lilmail
go build -o lilmail
```

## Minimal configuration

Create `config.toml` in the same directory as the binary:

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

> **Security note:** generate a strong `jwt.secret` (e.g.
> `openssl rand -hex 32`) and a random 32-byte `encryption.key` before
> deploying. Both protect session tokens and stored credentials at rest.

## First run

```bash
./lilmail
# or: go run main.go
```

Open **http://localhost:3000** in your browser. You will see the login page.
Enter your email address and mail server password — or click **Sign in with
OAuth2** if OAuth2 is configured.

## Common scenarios

### Self-signed TLS certificate

Set `insecure_skip_verify = true` under `[smtp]` (IMAP uses standard TLS on
port 993 and does not expose this flag):

```toml
[smtp]
insecure_skip_verify = true
```

### HTTPS with your own certificate

```toml
[ssl]
enabled    = true
cert_file  = "/etc/letsencrypt/live/yourdomain.com/fullchain.pem"
key_file   = "/etc/letsencrypt/live/yourdomain.com/privkey.pem"
port       = 443
http_port  = 80
auto_redirect = true
domain     = "yourdomain.com"
hsts_max_age = 31536000

[server]
secure_cookies = true
```

### OAuth2 / OpenID Connect

```toml
[oauth2]
enabled      = true
client_id    = "lilmail"
client_secret = "your-client-secret"
auth_url     = "https://auth.example.com/o/authorize/"
token_url    = "https://auth.example.com/o/token/"
redirect_url = "https://yourdomain.com/auth/oauth/callback"
scopes       = ["openid", "email", "profile"]
mechanism    = "xoauth2"
use_pkce     = true
```

Register `https://yourdomain.com/auth/oauth/callback` as a redirect URI with
your identity provider. Password login continues to work alongside OAuth2.

### Run behind a reverse proxy (nginx / Caddy)

lilmail trusts standard `X-Forwarded-*` headers. Configure your proxy to pass
them, set `[server] secure_cookies = true`, and let your proxy handle TLS
termination instead of using `[ssl]`.

## Verifying the installation

```bash
curl http://localhost:3000/health
# {"status":"ok"}
```

The `/health` endpoint returns `200 {"status":"ok"}` when the server is running.
It does not require authentication.

## Next steps

- Full configuration reference: [CONFIGURATION.md](CONFIGURATION.md)
- Code layout and architecture: [ARCHITECTURE.md](ARCHITECTURE.md)
- Feature roadmap: [../ROADMAP.md](../ROADMAP.md)
