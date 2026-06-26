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

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// Handler serves the JSON API. It owns no mail state of its own — every request
// reconstructs a MailClient from the caller's session via the AuthHandler.
type Handler struct {
	store  *session.Store
	config *config.Config
	auth   *web.AuthHandler
}

// New builds a JSON API handler. auth is the same *web.AuthHandler the HTMX UI
// uses, so both surfaces share one authentication + client-construction path.
func New(store *session.Store, cfg *config.Config, auth *web.AuthHandler) *Handler {
	return &Handler{store: store, config: cfg, auth: auth}
}

// Register mounts the API under /v1. Folder names travel as the `folder` query
// parameter (not a path segment) so names containing the IMAP hierarchy
// delimiter — e.g. "INBOX/Archive" — need no special escaping. UIDs are numeric
// and safe as path segments.
func (h *Handler) Register(app *fiber.App) {
	g := app.Group("/v1", h.requireAuth)

	g.Get("/me", h.handleMe)
	g.Get("/folders", h.handleFolders)
	g.Get("/messages", h.handleMessages)             // ?folder=&limit=
	g.Get("/messages/:uid", h.handleMessage)         // ?folder=
	g.Get("/search", h.handleSearch)                 // ?folder=&q=&limit=
	g.Patch("/messages/:uid/flags", h.handleSetFlag) // ?folder=  body {flag,add}
	g.Delete("/messages/:uid", h.handleDelete)       // ?folder=

	// Compose / drafts — JSON transport over the same SMTP/MIME engine as the
	// HTMX compose path. The :uid Delete above is registered first so it is not
	// shadowed; these add new paths.
	g.Post("/messages", h.handleSend)    // body {to, cc?, bcc?, subject, text?, html?, inReplyTo?}
	g.Post("/drafts", h.handleSaveDraft) // body {to, cc?, subject, text?, html?, inReplyTo?}

	// Calendar — only when CalDAV is enabled. Reuses the CalDAV client +
	// models.Calendar* types from the HTMX calendar surface.
	if h.config.CalDAV.Enabled {
		g.Get("/calendar/events", h.handleCalendarEvents)      // ?start=&end=
		g.Post("/calendar/events", h.handleCreateEvent)        // body {summary,start,end,...}
		g.Delete("/calendar/events/:uid", h.handleDeleteEvent) // by UID
		g.Get("/calendar/freebusy", h.handleFreeBusy)          // ?start=&end=
	}

	// Contacts — only when CardDAV is enabled. Reuses the CardDAV query path.
	if h.config.CardDAV.Enabled {
		g.Get("/contacts", h.handleContacts) // ?q=&limit=
	}
}

// requireAuth gates the group, returning 401 JSON (never a redirect) when the
// session is missing or unauthenticated.
func (h *Handler) requireAuth(c *fiber.Ctx) error {
	if _, err := api.ValidateSession(c, h.store); err != nil {
		return fail(c, fiber.StatusUnauthorized, "not authenticated")
	}
	return c.Next()
}

// client opens a MailClient for the current session and returns it; the caller
// must Close() it. Demo mode transparently yields the in-memory DemoClient.
func (h *Handler) client(c *fiber.Ctx) (api.MailClient, error) {
	return h.auth.CreateIMAPClient(c)
}

func (h *Handler) handleMe(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"email":    h.auth.GetSessionEmail(c),
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

func (h *Handler) handleMessages(c *fiber.Ctx) error {
	folder := folderParam(c)
	limit := uintQuery(c, "limit", 50)

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	emails, err := cl.FetchMessages(folder, limit)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not fetch messages")
	}
	return c.JSON(fiber.Map{"folder": folder, "messages": emails})
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

func (h *Handler) handleSetFlag(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")

	var body struct {
		Flag string `json:"flag"`
		Add  bool   `json:"add"`
	}
	if err := c.BodyParser(&body); err != nil || body.Flag == "" {
		return fail(c, fiber.StatusBadRequest, "body must be {flag, add}")
	}

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

	if err := cl.SetMessageFlag(folder, uid, body.Flag, body.Add); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not update flag")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) handleDelete(c *fiber.Ctx) error {
	folder := folderParam(c)
	uid := c.Params("uid")

	cl, err := h.client(c)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "mail server connection failed")
	}
	defer cl.Close()

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

func fail(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": msg})
}
