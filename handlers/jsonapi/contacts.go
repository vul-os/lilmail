// handlers/jsonapi/contacts.go — JSON contacts endpoints over CardDAV.
//
// Two surfaces share the same CardDAV backend:
//   - GET /contacts        — lean {email,name} search that powers compose
//     autocomplete (reuses api.CardDAVContacts, unchanged contract).
//   - GET /contacts/cards  — full models.Contact list for the contacts view.
//   - POST/PUT/DELETE      — create/update/delete a contact (vCard write).
//
// Each request picks its auth path the same way the calendar handlers do: a
// CP-brokered request uses the per-account X-Vulos-Mail-Carddav-Url + bearer
// token (never the session), while standalone lilmail uses the [carddav] config.
// A brokered account without a CardDAV URL degrades to an empty list / 501 on
// write, without ever touching the session config.
package jsonapi

import (
	"strings"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// handleContacts searches the configured CardDAV address book (lean form).
// GET /v1/contacts?q= → { contacts: [{ email, name }] }
func (h *Handler) handleContacts(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	limit := int(uintQuery(c, "limit", 50))

	var results []api.RecipientEntry
	if spec, ok := brokerSpecOf(c); ok {
		if spec.CardDAVURL != "" {
			results = brokerCardDAVContacts(spec, query, limit)
		}
		// else: brokered account without CardDAV → empty list (no session).
	} else {
		results = api.CardDAVContacts(
			h.config.CardDAV.URL,
			h.config.CardDAV.Username,
			h.config.CardDAV.Password,
			query,
			limit,
		)
	}

	type contact struct {
		Email string `json:"email"`
		Name  string `json:"name,omitempty"`
	}
	out := make([]contact, 0, len(results))
	for _, r := range results {
		out = append(out, contact{Email: r.Email, Name: r.Name})
	}
	return c.JSON(fiber.Map{"contacts": out})
}

// cardDAVAvailable reports whether the request has a usable CardDAV target and,
// for brokered requests, returns the spec. ok=false means "not available for this
// account" (brokered, no URL) or "not configured" (standalone).
func (h *Handler) cardDAVAvailable(c *fiber.Ctx) (spec brokerSpec, brokered, ok bool) {
	if s, isBroker := brokerSpecOf(c); isBroker {
		return s, true, s.CardDAVURL != ""
	}
	return brokerSpec{}, false, h.config.CardDAV.URL != ""
}

// handleContactCards lists full contact cards for the contacts view.
// GET /v1/contacts/cards?q=&limit= → { contacts: models.Contact[] }
func (h *Handler) handleContactCards(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	limit := int(uintQuery(c, "limit", 500))

	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return c.JSON(fiber.Map{"contacts": []models.Contact{}})
	}

	var (
		contacts []models.Contact
		err      error
	)
	if brokered {
		contacts, err = brokerContactsList(spec, query, limit)
	} else {
		contacts, err = api.ContactsList(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, query, limit)
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not list contacts")
	}
	if contacts == nil {
		contacts = []models.Contact{}
	}
	return c.JSON(fiber.Map{"contacts": contacts})
}

// contactBody is the JSON payload for creating/updating a contact.
type contactBody struct {
	UID    string   `json:"uid"`
	Name   string   `json:"name"`
	Org    string   `json:"org"`
	Title  string   `json:"title"`
	Note   string   `json:"note"`
	Emails []string `json:"emails"`
	Phones []string `json:"phones"`
	Path   string   `json:"path"`
}

func (b contactBody) toContact() models.Contact {
	return models.Contact{
		UID:    b.UID,
		Name:   strings.TrimSpace(b.Name),
		Org:    strings.TrimSpace(b.Org),
		Title:  strings.TrimSpace(b.Title),
		Note:   b.Note,
		Emails: b.Emails,
		Phones: b.Phones,
		Path:   b.Path,
	}
}

// handleCreateContact creates a contact.
// POST /v1/contacts  body {name, emails, phones?, org?, ...} → 201 { contact }
func (h *Handler) handleCreateContact(c *fiber.Ctx) error {
	var body contactBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	ct := body.toContact()
	if ct.Name == "" && len(ct.Emails) == 0 {
		return fail(c, fiber.StatusBadRequest, "a name or email is required")
	}
	ct.UID = "" // creation always mints a new UID; ignore any client-sent UID
	ct.Path = ""

	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}

	var (
		saved models.Contact
		err   error
	)
	if brokered {
		saved, err = brokerContactPut(spec, ct)
	} else {
		saved, err = api.ContactPut(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, ct)
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not create contact")
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"contact": saved})
}

// handleUpdateContact overwrites a contact (idempotent PUT).
// PUT /v1/contacts/:uid  body {name, emails, ...} → { contact }
func (h *Handler) handleUpdateContact(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "contact uid required")
	}
	var body contactBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	ct := body.toContact()
	ct.UID = uid
	if ct.Name == "" && len(ct.Emails) == 0 {
		return fail(c, fiber.StatusBadRequest, "a name or email is required")
	}

	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}

	var (
		saved models.Contact
		err   error
	)
	if brokered {
		saved, err = brokerContactPut(spec, ct)
	} else {
		saved, err = api.ContactPut(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, ct)
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not update contact")
	}
	return c.JSON(fiber.Map{"contact": saved})
}

// handleDeleteContact removes a contact by UID (optionally ?path= to target the
// exact object). DELETE /v1/contacts/:uid → 204
func (h *Handler) handleDeleteContact(c *fiber.Ctx) error {
	uid := c.Params("uid")
	if uid == "" {
		return fail(c, fiber.StatusBadRequest, "contact uid required")
	}
	objPath := c.Query("path")

	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}

	var err error
	if brokered {
		err = brokerContactDelete(spec, uid, objPath)
	} else {
		err = api.ContactDelete(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, uid, objPath)
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not delete contact")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
