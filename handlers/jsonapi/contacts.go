// handlers/jsonapi/contacts.go — JSON contacts endpoint over CardDAV.
//
// Reuses the same CardDAV query path (api.CardDAVContacts) that powers the HTMX
// compose autocomplete (handlers/web/email.go HandleAutocomplete). Registered
// only when [carddav] enabled = true.
package jsonapi

import (
	"strings"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// handleContacts searches the configured CardDAV address book.
// GET /v1/contacts?q= → { contacts: [{ email, name }] }
// An empty q returns up to `limit` contacts.
//
// For CP-brokered requests the address book is queried from the per-account
// X-Vulos-Mail-Carddav-Url + bearer-token headers (never the session config). A
// brokered account without a CardDAV URL returns an empty list.
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
