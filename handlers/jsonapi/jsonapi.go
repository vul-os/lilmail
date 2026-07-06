// Package jsonapi exposes lilmail's mail engine as a clean JSON/REST API,
// separate from the HTMX/Alpine web UI in handlers/web.
//
// WHY THIS EXISTS: the handlers/web routes render HTML fragments for HTMX. That
// is perfect for the zero-build single-binary standalone UI, but a rich React
// client (Vulos Mail's webmail and Vulos Workspace's mail surface) needs a
// stable machine-readable contract. This package is that contract — it returns
// models.Email / MailboxInfo as JSON and never renders templates.
//
// It is purely additive: it reuses the SAME engine (handlers/api) and the SAME
// session auth path (web.AuthHandler.CreateIMAPClient) as the HTMX UI, so there
// is no duplicated mail logic and the existing UI is untouched. Removing this
// package leaves standalone lilmail fully working.
//
// Auth: unlike the HTMX SessionMiddleware (which 302-redirects to /login), this
// API returns 401 JSON so a fetch()-based client can handle it.
package jsonapi

import (
	"strconv"
	"strings"
	"time"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// Handler serves the JSON API. It owns no mail state of its own — every request
// reconstructs a MailClient from the caller's session via the AuthHandler, or
// (in CP-brokered mode) directly from validated X-Vulos-Mail-* request headers.
type Handler struct {
	store  *session.Store
	config *config.Config
	auth   *web.AuthHandler
	// brokerSecret gates the CP-brokered credential path (see broker.go). When
	// empty, brokered headers are never trusted and the API behaves identically
	// to standalone lilmail. Read once from LILMAIL_BROKER_SECRET at construction.
	brokerSecret string
}

// New builds a JSON API handler. auth is the same *web.AuthHandler the HTMX UI
// uses, so both surfaces share one authentication + client-construction path.
// The CP-brokered credential mode is enabled when LILMAIL_BROKER_SECRET is set.
func New(store *session.Store, cfg *config.Config, auth *web.AuthHandler) *Handler {
	return &Handler{store: store, config: cfg, auth: auth, brokerSecret: readBrokerSecret()}
}

// Register mounts the API under /v1. Folder names travel as the `folder` query
// parameter (not a path segment) so names containing the IMAP hierarchy
// delimiter — e.g. "INBOX/Archive" — need no special escaping. UIDs are numeric
// and safe as path segments.
func (h *Handler) Register(app *fiber.App) {
	// brokerMiddleware runs first: it validates the broker secret and, if valid,
	// parses the X-Vulos-Mail-* headers into a connection spec for this request.
	// requireAuth then accepts either a brokered request or a valid session.
	g := app.Group("/v1", h.brokerMiddleware, h.requireAuth)

	g.Get("/me", h.handleMe)
	g.Get("/folders", h.handleFolders)
	g.Get("/messages", h.handleMessages)     // ?folder=&limit=
	g.Get("/messages/:uid", h.handleMessage) // ?folder=
	// Attachment download — streams a single MIME part. Works in both session and
	// CP-brokered modes; engages the object-storage attachment cache when present.
	g.Get("/messages/:uid/attachments/:partId", h.handleAttachment) // ?folder=
	g.Get("/search", h.handleSearch)                                // ?folder=&q=&limit=
	g.Patch("/messages/:uid/flags", h.handleSetFlag)                // ?folder=  body {flag,add}
	g.Delete("/messages/:uid", h.handleDelete)                      // ?folder=&hard=
	g.Post("/messages/:uid/move", h.handleMove)                     // ?folder=  body {toFolder, folder?}
	g.Post("/messages/:uid/snooze", h.handleSnooze)                 // ?folder=  body {until}
	g.Delete("/messages/:uid/snooze", h.handleUnsnooze)             // ?folder=  (cancel scheduled return)

	// Folder (label) create/delete + report-spam. Plain IMAP operations, so they
	// work in both session and brokered modes. System folders are delete-protected.
	h.registerFolders(g)

	// Compose / drafts — JSON transport over the same SMTP/MIME engine as the
	// HTMX compose path. The :uid Delete above is registered first so it is not
	// shadowed; these add new paths.
	//
	// POST /v1/messages is rate-limited per IP to prevent spam/relay abuse.
	// The limit is read from [rate_limit] in config.toml (default 30/60 s).
	sendLimiter := limiter.New(limiter.Config{
		Max:        h.config.RateLimit.SendMax,
		Expiration: time.Duration(h.config.RateLimit.SendWindow) * time.Second,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return fail(c, fiber.StatusTooManyRequests, "rate limit exceeded")
		},
	})
	g.Post("/messages", sendLimiter, h.handleSend) // body {to, cc?, bcc?, subject, text?, html?, inReplyTo?, attachments?}
	g.Post("/drafts", h.handleSaveDraft)           // body {to, cc?, subject, text?, html?, inReplyTo?, attachments?}

	// Attachment staging for compose: upload a file, get back a token to reference
	// in the attachments array of a later /v1/messages or /v1/drafts POST.
	g.Post("/attachments", h.handleUploadAttachment) // multipart form, file field "file"

	// Calendar — registered when CalDAV is enabled OR the broker path is active.
	// In CP-brokered deployments the per-account CalDAV URL arrives per request
	// (X-Vulos-Mail-Caldav-Url), so the routes must exist even when the local
	// [caldav] block is not enabled; the brokered handlers build the client from
	// the headers. Reuses the CalDAV client + models.Calendar* types from the
	// HTMX calendar surface.
	if h.config.CalDAV.Enabled || h.brokerSecret != "" {
		g.Get("/calendar/events", h.handleCalendarEvents)      // ?start=&end=
		g.Post("/calendar/events", h.handleCreateEvent)        // body {summary,start,end,...}
		g.Put("/calendar/events/:uid", h.handleUpdateEvent)    // body {summary,start,end,...}
		g.Delete("/calendar/events/:uid", h.handleDeleteEvent) // by UID
		g.Get("/calendar/freebusy", h.handleFreeBusy)          // ?start=&end=
		// iTIP RSVP: reply to a received invitation (sends METHOD:REPLY + reflects
		// the event in the responder's own calendar). body {uid, organizer, response, ...event}
		g.Post("/calendar/rsvp", h.handleCalendarRSVP)
	}

	// Contacts — registered when CardDAV is enabled OR the broker path is active
	// (per-account X-Vulos-Mail-Carddav-Url arrives per request). Reuses the
	// CardDAV query path.
	if h.config.CardDAV.Enabled || h.brokerSecret != "" {
		g.Get("/contacts", h.handleContacts)            // ?q=&limit=  (lean autocomplete form)
		g.Get("/contacts/cards", h.handleContactCards)  // ?q=&limit=  (full cards)
		g.Post("/contacts", h.handleCreateContact)      // body {name,emails,...}
		g.Put("/contacts/:uid", h.handleUpdateContact)  // body {name,emails,...}
		g.Delete("/contacts/:uid", h.handleDeleteContact) // ?path=
	}

	// Rules / filters — registered when the broker path is active (the authoritative
	// per-account rule store lives in vulos-mail and its base URL arrives per
	// request as X-Vulos-Mail-Rules-Url). When a request carries no rule-store URL
	// (e.g. a plain Gmail/IMAP brokered account), the handlers report 501 and the
	// mail-ui hides Filters. See rules.go.
	if h.brokerSecret != "" {
		h.registerRules(g)
	}
}

// requireAuth gates the group, returning 401 JSON (never a redirect) when the
// request is neither a validated CP-brokered request nor a valid session. The
// broker middleware has already run, so a brokered request is trusted here.
func (h *Handler) requireAuth(c *fiber.Ctx) error {
	if _, ok := brokerSpecOf(c); ok {
		return c.Next()
	}
	if _, err := api.ValidateSession(c, h.store); err != nil {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	return c.Next()
}

// client opens a MailClient for the request and returns it; the caller must
// Close() it. For CP-brokered requests the client is built directly from the
// validated X-Vulos-Mail-* headers; otherwise it comes from the session via the
// AuthHandler. Demo mode transparently yields the in-memory DemoClient.
func (h *Handler) client(c *fiber.Ctx) (api.MailClient, error) {
	if spec, ok := brokerSpecOf(c); ok {
		return brokerDialIMAP(spec)
	}
	return h.auth.CreateIMAPClient(c)
}

// smtpSender is the subset of *api.SMTPClient the send path uses. Declaring it
// here lets the brokered SMTP builder be swapped in tests (via brokerSMTPSender)
// so the compose/send flow — including attachment assembly — can be exercised
// without a live SMTP server. Both *api.SMTPClient and the session client satisfy it.
type smtpSender interface {
	SendRawMessage(allRcpts []string, rawMessage []byte) error
}

// brokerSMTPSender builds the brokered SMTP sender. Package var for the test seam.
var brokerSMTPSender = func(spec brokerSpec) smtpSender { return brokerSMTPClient(spec) }

// smtpClient returns an SMTP sender for the request: brokered SMTP host/port +
// creds for CP-brokered requests, otherwise the session-derived client.
func (h *Handler) smtpClient(c *fiber.Ctx) (smtpSender, error) {
	if spec, ok := brokerSpecOf(c); ok {
		return brokerSMTPSender(spec), nil
	}
	return h.auth.CreateSMTPClient(c)
}

// fromEmail returns the sender identity for the request: the brokered mailbox
// address for CP-brokered requests, otherwise the session email.
func (h *Handler) fromEmail(c *fiber.Ctx) string {
	if spec, ok := brokerSpecOf(c); ok {
		return spec.Email
	}
	return h.auth.GetSessionEmail(c)
}

func (h *Handler) handleMe(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"email":    h.fromEmail(c),
		"username": c.Locals("username"),
	})
}

func (h *Handler) handleFolders(c *fiber.Ctx) error {
	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	folders, err := cl.FetchFolders()
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not list folders")
	}
	return c.JSON(fiber.Map{"folders": folders})
}

// handleMessages lists a folder newest-first. `limit` caps the page size and
// `offset` skips the newest N messages, so a client can scroll-load a large
// mailbox: page k = ?limit=L&offset=k*L. The response echoes the effective
// limit/offset and sets nextOffset to the offset for the following page (null
// when the returned page was smaller than the limit, i.e. no more to fetch).
func (h *Handler) handleMessages(c *fiber.Ctx) error {
	folder := folderParam(c)
	limit := uintQuery(c, "limit", 50)
	offset := uintQuery(c, "offset", 0)

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	emails, err := cl.FetchMessagesPaged(folder, limit, offset)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not fetch messages")
	}

	// nextOffset advances the window only when this page was full; a short page
	// means the end of the mailbox was reached, so there is nothing more to load.
	var nextOffset interface{}
	if limit > 0 && uint32(len(emails)) >= limit {
		nextOffset = offset + limit
	}
	return c.JSON(fiber.Map{
		"folder":     folder,
		"limit":      limit,
		"offset":     offset,
		"nextOffset": nextOffset,
		"messages":   emails,
	})
}

func (h *Handler) handleMessage(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	email, err := cl.FetchSingleMessage(folder, uid)
	if err != nil {
		return fail(c, fiber.StatusNotFound, "message not found")
	}
	// Refine the invite's MyPartStat using the authoritative sending identity
	// (fromEmail) rather than the raw IMAP login username used during parse.
	if email.Invite != nil {
		me := strings.ToLower(strings.TrimSpace(h.fromEmail(c)))
		email.Invite.MyPartStat = ""
		for _, a := range email.Invite.Attendees {
			if strings.ToLower(strings.TrimSpace(a.Email)) == me {
				email.Invite.MyPartStat = a.PartStat
				break
			}
		}
	}
	return c.JSON(email)
}

func (h *Handler) handleSearch(c *fiber.Ctx) error {
	folder := folderParam(c)
	query := c.Query("q")
	if query == "" {
		return fail(c, fiber.StatusBadRequest, "missing q")
	}
	limit := uintQuery(c, "limit", 100)

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	emails, err := cl.SearchMessages(folder, query, limit)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "search failed")
	}
	return c.JSON(fiber.Map{"folder": folder, "query": query, "messages": emails})
}

// handleSetFlag adds or removes IMAP flags/keywords on a message. It accepts a
// single flag ({flag, add}) or a batch ({flags:[...], add}) — the two forms are
// merged. Beyond the system flags (\Seen, \Flagged, \Answered, \Deleted, …) this
// applies arbitrary custom KEYWORDS, which is how a client sets user labels
// (e.g. "Important", "$Label1") or a "snoozed" marker: the mechanism is standard
// IMAP STORE with a keyword atom. Whether custom keywords PERSIST is a
// server-side property — the mailbox must advertise PERMANENTFLAGS containing
// \* (most modern servers, incl. Dovecot/Gmail, do). If it does not, the STORE
// is accepted for the session but not retained; that is the server's limitation,
// not the API's, and we surface the server's response as-is.
// PATCH /v1/messages/:uid/flags  ?folder=  body {flag|flags[], add}
func (h *Handler) handleSetFlag(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")

	var body struct {
		Flag  string   `json:"flag"`
		Flags []string `json:"flags"`
		Add   bool     `json:"add"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "body must be {flag|flags[], add}")
	}

	// Merge single + batch forms, de-duplicating and dropping blanks.
	seen := map[string]bool{}
	var flags []string
	for _, f := range append(body.Flags, body.Flag) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		flags = append(flags, f)
	}
	if len(flags) == 0 {
		return fail(c, fiber.StatusBadRequest, "at least one flag is required")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	for _, f := range flags {
		if err := cl.SetMessageFlag(folder, uid, f, body.Add); err != nil {
			return fail(c, fiber.StatusBadGateway, "could not update flag")
		}
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleDelete deletes a message. By default it MOVES the message to the Trash
// folder (discovered via the \Trash special-use). With ?hard=true (or =1) it
// permanently expunges the message. If the source folder already IS the Trash
// folder, or no Trash folder can be found, it falls back to a hard delete.
// DELETE /v1/messages/:uid  ?folder=&hard=
func (h *Handler) handleDelete(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")
	hard := boolQuery(c, "hard")

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	if !hard {
		// Soft delete: move to Trash when we can resolve a distinct Trash folder.
		if trash, terr := cl.DiscoverTrashFolder(); terr == nil &&
			trash != "" && !strings.EqualFold(trash, folder) {
			if err := cl.MoveMessage(folder, uid, trash); err != nil {
				return fail(c, fiber.StatusBadGateway, "could not delete message")
			}
			return c.SendStatus(fiber.StatusNoContent)
		}
		// No usable Trash folder (or already in Trash): fall through to hard delete.
	}

	if err := cl.DeleteMessage(folder, uid); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not delete message")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// folderParam returns the requested folder, defaulting to INBOX.
func folderParam(c *fiber.Ctx) string {
	if f := c.Query("folder"); f != "" {
		return f
	}
	return "INBOX"
}

func uintQuery(c *fiber.Ctx, key string, def uint32) uint32 {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return def
	}
	return uint32(n)
}

// boolQuery parses a query param as a boolean flag: "1" or "true"
// (case-insensitive) => true; anything else (including absent) => false.
func boolQuery(c *fiber.Ctx, key string) bool {
	v := strings.ToLower(c.Query(key))
	return v == "1" || v == "true"
}

func fail(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": msg})
}

// failErr renders an error as JSON. A *fiber.Error keeps its status + message;
// anything else becomes a generic 500. Lets helpers return typed errors that the
// handler surfaces uniformly (see resolveAttachments).
func failErr(c *fiber.Ctx, err error) error {
	if fe, ok := err.(*fiber.Error); ok {
		return fail(c, fe.Code, fe.Message)
	}
	return fail(c, fiber.StatusInternalServerError, "internal error")
}
