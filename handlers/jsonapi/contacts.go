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
// GET /v1/contacts/cards?q=&limit=&group= → { contacts: models.Contact[] }
// When group= is set, only cards whose CATEGORIES contain that group are returned
// (case-insensitive), giving the client a per-account group filter.
func (h *Handler) handleContactCards(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	group := strings.TrimSpace(c.Query("group"))
	limit := int(uintQuery(c, "limit", 500))

	contacts, err := h.listAllContacts(c, query, limit)
	if err == errContactsUnavailable {
		return c.JSON(fiber.Map{"contacts": []models.Contact{}})
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not list contacts")
	}
	if group != "" {
		contacts = filterByGroup(contacts, group)
	} else {
		contacts = withoutPlaceholders(contacts)
	}
	if contacts == nil {
		contacts = []models.Contact{}
	}
	return c.JSON(fiber.Map{"contacts": contacts})
}

// errContactsUnavailable signals the account has no usable CardDAV target, so the
// caller degrades to an empty list rather than a 502.
var errContactsUnavailable = errContacts("contacts not available for this account")

type errContacts string

func (e errContacts) Error() string { return string(e) }

// listAllContacts is the shared read path used by the cards list, groups
// aggregation and export. It routes through the same brokered/standalone seam so
// per-account isolation is identical to every other contacts surface.
func (h *Handler) listAllContacts(c *fiber.Ctx, query string, limit int) ([]models.Contact, error) {
	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return nil, errContactsUnavailable
	}
	if brokered {
		return brokerContactsList(spec, query, limit)
	}
	return api.ContactsList(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, query, limit)
}

// putContact routes a create/update through the same brokered/standalone seam as
// the handlers, so groups + import reuse one write path with identical isolation.
func (h *Handler) putContact(c *fiber.Ctx, ct models.Contact) (models.Contact, error) {
	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return models.Contact{}, errContactsUnavailable
	}
	if brokered {
		return brokerContactPut(spec, ct)
	}
	return api.ContactPut(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, ct)
}

// deleteContact routes a delete through the brokered/standalone seam.
func (h *Handler) deleteContact(c *fiber.Ctx, uid, objPath string) error {
	spec, brokered, ok := h.cardDAVAvailable(c)
	if !ok {
		return errContactsUnavailable
	}
	if brokered {
		return brokerContactDelete(spec, uid, objPath)
	}
	return api.ContactDelete(h.config.CardDAV.URL, h.config.CardDAV.Username, h.config.CardDAV.Password, uid, objPath)
}

// withoutPlaceholders drops internal group-placeholder cards so they never
// surface in the contacts list.
func withoutPlaceholders(in []models.Contact) []models.Contact {
	out := make([]models.Contact, 0, len(in))
	for _, ct := range in {
		if !isPlaceholderGroupCard(ct) {
			out = append(out, ct)
		}
	}
	return out
}

// filterByGroup keeps only contacts whose Groups contain group (case-insensitive).
func filterByGroup(in []models.Contact, group string) []models.Contact {
	want := strings.ToLower(group)
	out := make([]models.Contact, 0, len(in))
	for _, ct := range in {
		if isPlaceholderGroupCard(ct) {
			continue // internal group placeholder is never a real member
		}
		for _, g := range ct.Groups {
			if strings.ToLower(strings.TrimSpace(g)) == want {
				out = append(out, ct)
				break
			}
		}
	}
	return out
}

// contactBody is the JSON payload for creating/updating a contact. It mirrors
// models.Contact so the full field depth (structured name, TYPE labels, ADR,
// birthday, websites, IM, department, groups) round-trips; the flat Emails/Phones
// remain accepted for the legacy/lean client.
type contactBody struct {
	UID            string                 `json:"uid"`
	Name           string                 `json:"name"`
	StructuredName *models.StructuredName `json:"structuredName"`
	Nickname       string                 `json:"nickname"`
	FileAs         string                 `json:"fileAs"`
	Org            string                 `json:"org"`
	Department     string                 `json:"department"`
	Title          string                 `json:"title"`
	Note           string                 `json:"note"`
	Emails         []string               `json:"emails"`
	Phones         []string               `json:"phones"`
	TypedEmails    []models.TypedValue    `json:"typedEmails"`
	TypedPhones    []models.TypedValue    `json:"typedPhones"`
	Addresses      []models.Address       `json:"addresses"`
	Websites       []models.TypedValue    `json:"websites"`
	IMs            []models.TypedValue    `json:"ims"`
	Birthday       string                 `json:"birthday"`
	Anniversary    string                 `json:"anniversary"`
	Groups         []string               `json:"groups"`
	Photo          string                 `json:"photo"`
	Starred        bool                   `json:"starred"`
	Path           string                 `json:"path"`
}

func (b contactBody) toContact() models.Contact {
	return sanitizeContact(models.Contact{
		UID:            b.UID,
		Name:           strings.TrimSpace(b.Name),
		StructuredName: b.StructuredName,
		Nickname:       strings.TrimSpace(b.Nickname),
		FileAs:         strings.TrimSpace(b.FileAs),
		Org:            strings.TrimSpace(b.Org),
		Department:     strings.TrimSpace(b.Department),
		Title:          strings.TrimSpace(b.Title),
		Note:           b.Note,
		Emails:         b.Emails,
		Phones:         b.Phones,
		TypedEmails:    b.TypedEmails,
		TypedPhones:    b.TypedPhones,
		Addresses:      b.Addresses,
		Websites:       b.Websites,
		IMs:            b.IMs,
		Birthday:       strings.TrimSpace(b.Birthday),
		Anniversary:    strings.TrimSpace(b.Anniversary),
		Groups:         b.Groups,
		Photo:          b.Photo,
		Starred:        b.Starred,
		Path:           b.Path,
	})
}

// handleCreateContact creates a contact.
// POST /v1/contacts  body {name, emails, phones?, org?, ...} → 201 { contact }
func (h *Handler) handleCreateContact(c *fiber.Ctx) error {
	var body contactBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	ct := body.toContact()
	if !hasIdentity(ct) {
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
	if !hasIdentity(ct) {
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
