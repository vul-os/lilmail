// handlers/jsonapi/broker.go — CP-brokered credential mode for the /v1 JSON API.
//
// WHY THIS EXISTS: Vulos Cloud's control plane (CP) custodies users' EXTERNAL
// mailbox credentials (Gmail/Outlook/IMAP) and reverse-proxies to lilmail's /v1
// surface, injecting the per-request connection credentials as HTTP headers. In
// that deployment lilmail has no session of its own — the mailbox identity and
// secret arrive on every request. This file lets lilmail consume those headers
// and build a MailClient directly from them, instead of the normal
// session→CreateIMAPClient path.
//
// SECURITY: this is credential injection if done wrong. The X-Vulos-Mail-*
// headers are honored ONLY when the request also presents a valid broker secret
// (X-Vulos-Broker-Auth, constant-time compared against LILMAIL_BROKER_SECRET).
// If LILMAIL_BROKER_SECRET is unset, or the presented secret does not match, the
// brokered headers are IGNORED ENTIRELY and the request falls back to normal
// session auth. Standalone lilmail (no secret configured) therefore never trusts
// arbitrary client headers. The headers are only ever read inside the /v1 group
// (after the broker middleware), never on unauthenticated or HTMX paths.
package jsonapi

import (
	"context"
	"crypto/subtle"
	"os"
	"strconv"
	"strings"
	"time"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// Broker request headers. The CP sends these on every proxied /v1 request.
const (
	hdrBrokerAuth = "X-Vulos-Broker-Auth"

	hdrMailProvider = "X-Vulos-Mail-Provider"
	hdrMailEmail    = "X-Vulos-Mail-Email"
	hdrMailUsername = "X-Vulos-Mail-Username"
	hdrMailAuth     = "X-Vulos-Mail-Auth"
	hdrMailSecret   = "X-Vulos-Mail-Secret"
	hdrMailIMAPHost = "X-Vulos-Mail-Imap-Host"
	hdrMailIMAPPort = "X-Vulos-Mail-Imap-Port"
	hdrMailSMTPHost = "X-Vulos-Mail-Smtp-Host"
	hdrMailSMTPPort = "X-Vulos-Mail-Smtp-Port"

	// CalDAV/CardDAV base URLs for the brokered account. Optional: the CP only
	// sends them for accounts that actually expose DAV (e.g. Gmail/IMAP). When
	// absent, the calendar/contacts routes report "not available for this
	// account" rather than touching the session. Auth reuses X-Vulos-Mail-Auth=
	// xoauth2 + X-Vulos-Mail-Secret as an HTTP Bearer token.
	hdrMailCalDAVURL  = "X-Vulos-Mail-Caldav-Url"
	hdrMailCardDAVURL = "X-Vulos-Mail-Carddav-Url"
)

// brokerEnvSecret is the env var that gates the whole brokered path. When empty,
// brokered headers are never trusted (standalone lilmail behaviour).
const brokerEnvSecret = "LILMAIL_BROKER_SECRET"

// brokerLocalsKey is the Fiber Locals key under which a validated brokerSpec is
// stashed for the duration of a request.
const brokerLocalsKey = "vulosBrokerSpec"

// Auth mechanisms understood by the brokered path.
const (
	brokerAuthXOAuth2 = "xoauth2"
	brokerAuthPlain   = "plain"
)

// brokerSpec is the per-request external-mailbox connection descriptor parsed
// from the X-Vulos-Mail-* headers. It is only ever constructed after the broker
// secret has been validated.
type brokerSpec struct {
	Provider string // gmail | outlook | imap (informational)
	Email    string // mailbox address, used as MAIL FROM / identity
	Username string // IMAP/SMTP login username (defaults to Email)
	Auth     string // xoauth2 | plain
	Secret   string // XOAUTH2 access token, or IMAP/SMTP password
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
	// CalDAVURL / CardDAVURL are the per-account DAV base URLs. Empty when the
	// brokered account has no calendar/contacts surface (e.g. plain IMAP).
	CalDAVURL  string
	CardDAVURL string
}

// brokerDialIMAP builds a live MailClient from a validated broker spec. It is a
// package var so tests can substitute a mock and assert on the spec without a
// live IMAP server.
var brokerDialIMAP = func(spec brokerSpec) (api.MailClient, error) {
	if spec.Auth == brokerAuthXOAuth2 {
		return api.NewClientOAuth(spec.IMAPHost, spec.IMAPPort, spec.Username, spec.Secret, brokerAuthXOAuth2)
	}
	return api.NewClient(spec.IMAPHost, spec.IMAPPort, spec.Username, spec.Secret)
}

// brokerSMTPClient builds an SMTP client from a validated broker spec. Port 465
// implies implicit TLS; anything else (typically 587) uses STARTTLS.
func brokerSMTPClient(spec brokerSpec) *api.SMTPClient {
	useStartTLS := spec.SMTPPort != 465
	if spec.Auth == brokerAuthXOAuth2 {
		return api.NewSMTPClientOAuth(spec.SMTPHost, spec.SMTPPort, spec.Email, spec.Secret, brokerAuthXOAuth2, useStartTLS)
	}
	return api.NewSMTPClient(spec.SMTPHost, spec.SMTPPort, spec.Email, spec.Secret, useStartTLS)
}

// calDAVClient is the subset of *api.CalDAVClient the JSON calendar handlers use.
// Declaring it here lets the brokered dial seam be mocked in tests without a live
// CalDAV server; *api.CalDAVClient (and the session path) satisfy it directly.
type calDAVClient interface {
	ListEvents(ctx context.Context, start, end time.Time) ([]models.CalendarEvent, error)
	CreateEvent(ctx context.Context, ev models.CalendarEvent) error
	DeleteEvent(ctx context.Context, uid string) error
	FreeBusy(ctx context.Context, start, end time.Time) ([]models.FreeBusySlot, error)
}

// brokerDialCalDAV builds a CalDAV client from a validated broker spec, using the
// account's CalDAV base URL and the XOAUTH2 access token as an HTTP Bearer token.
// It reuses api.NewCalDAVClient's oauth2/bearer mode (handlers/api/caldav.go); no
// new DAV library is introduced. It is a package var so tests can substitute a
// mock and assert on the spec without a live CalDAV server.
var brokerDialCalDAV = func(spec brokerSpec) (calDAVClient, error) {
	cfg := config.CalDAVConfig{
		Enabled: true,
		URL:     spec.CalDAVURL,
		Auth:    "oauth2",
	}
	return api.NewCalDAVClient(cfg, spec.Secret)
}

// brokerCardDAVContacts queries the brokered account's CardDAV address book using
// the CardDAV base URL and the XOAUTH2 access token as an HTTP Bearer token. It
// reuses the CardDAV query path in handlers/api. Package var for test seam.
var brokerCardDAVContacts = func(spec brokerSpec, query string, limit int) []api.RecipientEntry {
	return api.CardDAVContactsBearer(spec.CardDAVURL, spec.Secret, query, limit)
}

// brokerMiddleware validates the broker secret and, if valid, parses the
// X-Vulos-Mail-* headers into a brokerSpec stashed in Locals. When the secret is
// absent/invalid, or the headers are incomplete, it is a no-op and the request
// continues to normal session auth (requireAuth). It NEVER rejects a request on
// its own — that keeps standalone behaviour identical.
func (h *Handler) brokerMiddleware(c *fiber.Ctx) error {
	if spec, ok := h.parseBroker(c); ok {
		c.Locals(brokerLocalsKey, spec)
		// Make the brokered identity visible to downstream best-effort code
		// (e.g. recipient autocomplete) that reads the username local.
		c.Locals("username", spec.Username)
	}
	return c.Next()
}

// parseBroker checks the broker secret with a constant-time compare and, on
// success, parses the mailbox headers. It returns ok=false (and the headers are
// ignored) whenever the gate is closed: secret unset, missing/mismatched auth
// header, an unknown auth mechanism, or essential mailbox fields missing.
func (h *Handler) parseBroker(c *fiber.Ctx) (brokerSpec, bool) {
	secret := h.brokerSecret
	if secret == "" {
		return brokerSpec{}, false // gate disabled — never trust headers
	}

	presented := c.Get(hdrBrokerAuth)
	if presented == "" {
		return brokerSpec{}, false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
		return brokerSpec{}, false
	}

	spec := brokerSpec{
		Provider: c.Get(hdrMailProvider),
		Email:    strings.TrimSpace(c.Get(hdrMailEmail)),
		Username: strings.TrimSpace(c.Get(hdrMailUsername)),
		Auth:     strings.ToLower(strings.TrimSpace(c.Get(hdrMailAuth))),
		Secret:   c.Get(hdrMailSecret),
		IMAPHost: strings.TrimSpace(c.Get(hdrMailIMAPHost)),
		IMAPPort: atoiDefault(c.Get(hdrMailIMAPPort), 993),
		SMTPHost: strings.TrimSpace(c.Get(hdrMailSMTPHost)),
		SMTPPort: atoiDefault(c.Get(hdrMailSMTPPort), 587),
		// Optional DAV URLs — never required to validate the spec.
		CalDAVURL:  strings.TrimSpace(c.Get(hdrMailCalDAVURL)),
		CardDAVURL: strings.TrimSpace(c.Get(hdrMailCardDAVURL)),
	}

	if spec.Auth == "" {
		spec.Auth = brokerAuthPlain
	}
	if spec.Auth != brokerAuthXOAuth2 && spec.Auth != brokerAuthPlain {
		return brokerSpec{}, false
	}
	// Essential fields: without these we cannot build a client, so fall back to
	// session auth rather than half-trusting partial headers.
	if spec.Email == "" || spec.Secret == "" || spec.IMAPHost == "" {
		return brokerSpec{}, false
	}
	if spec.Username == "" {
		spec.Username = spec.Email
	}
	if spec.SMTPHost == "" {
		spec.SMTPHost = spec.IMAPHost
	}
	return spec, true
}

// brokerSpecOf returns the validated brokerSpec for the request, if the broker
// path is active for it.
func brokerSpecOf(c *fiber.Ctx) (brokerSpec, bool) {
	spec, ok := c.Locals(brokerLocalsKey).(brokerSpec)
	return spec, ok
}

// atoiDefault parses s as an int, returning def for empty/invalid input.
func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// readBrokerSecret returns the configured broker secret from the environment.
func readBrokerSecret() string {
	return strings.TrimSpace(os.Getenv(brokerEnvSecret))
}
