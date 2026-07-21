# Security Policy

lilmail is a lightweight, database-free PIM client — mail, calendar and contacts
— in a single Go binary. It handles account credentials and renders untrusted
message content, so its security boundary matters. Reports are taken seriously
and handled with priority.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

- Preferred: [GitHub private vulnerability reporting](https://github.com/vul-os/lilmail/security/advisories/new) on `vul-os/lilmail`.
- Alternatively, email **vulosorg@gmail.com** with `[lilmail security]` in the subject.

Include what you can: affected area (credential/token handling, message
rendering, calendar/contacts sync), reproduction steps, and impact as you
understand it. You'll get an acknowledgement within **72 hours** and a status
update at least every **14 days** until resolution. Please give a reasonable
window to ship a fix before public disclosure — we'll credit you in the release
notes unless you'd rather stay anonymous.

## Scope

Especially interested in:

- **Credential & token handling** — any path that displays, logs, or exfiltrates
  account passwords or OAuth tokens, or mishandles them at rest.
- **Message rendering** — HTML/script injection (XSS), remote-content leaks that
  deanonymize the reader, or spoofing that misrepresents a message's sender.
- **Egress** — any network call beyond the mail/calendar/contacts servers the
  user configured.
- **Calendar & contacts sync** — malformed server data corrupting local state or
  escaping the intended parse boundary.

Out of scope: vulnerabilities requiring an already-compromised host, and issues
in third-party services the user configures (their mail/calendar provider).

## Supported versions

Only the latest release (and `main`) receives fixes.
