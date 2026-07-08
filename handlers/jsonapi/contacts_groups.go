// handlers/jsonapi/contacts_groups.go — contact groups (labels) over CardDAV
// CATEGORIES. Groups are modelled entirely inside each card's CATEGORIES
// property (the Google/Apple X-ADDRESSBOOKSERVER convention), so there is no
// separate group store to keep per-account: a group EXISTS iff some card in the
// account's book carries it. This keeps groups per-account by construction —
// they live in the same address book the contacts CRUD already isolates.
//
//	GET    /v1/contacts/groups            → { groups: [{name, count}] }
//	POST   /v1/contacts/groups            body {name}            → create (adds an
//	                                        empty placeholder membership so the
//	                                        group is visible before any assignment)
//	PATCH  /v1/contacts/groups/:name      body {name}            → rename across cards
//	DELETE /v1/contacts/groups/:name                              → remove from all cards
//
// Assignment/unassignment of a single contact is done through the normal contact
// PUT (the contact's `groups` array is authoritative), so there is no separate
// assign endpoint — the client edits contact.groups and saves.
package jsonapi

import (
	"sort"
	"strings"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// maxGroupNameLen bounds an untrusted group name on create/rename.
const maxGroupNameLen = 128

// registerContactGroups mounts the group routes. Called from Register alongside
// the other contacts routes so they share the same CardDAV availability gate.
func (h *Handler) registerContactGroups(g fiber.Router) {
	g.Get("/contacts/groups", h.handleListGroups)
	g.Post("/contacts/groups", h.handleCreateGroup)
	g.Patch("/contacts/groups/:name", h.handleRenameGroup)
	g.Delete("/contacts/groups/:name", h.handleDeleteGroup)
}

// handleListGroups aggregates the distinct CATEGORIES across the account's book
// with a per-group contact count. Empty/placeholder memberships are excluded from
// the count but still surface the group name.
func (h *Handler) handleListGroups(c *fiber.Ctx) error {
	contacts, err := h.listAllContacts(c, "", 0)
	if err == errContactsUnavailable {
		return c.JSON(fiber.Map{"groups": []groupInfo{}})
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not list groups")
	}
	counts := map[string]int{}   // lowercase -> count
	display := map[string]string{} // lowercase -> first-seen display name
	for _, ct := range contacts {
		for _, g := range ct.Groups {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			key := strings.ToLower(g)
			if _, ok := display[key]; !ok {
				display[key] = g
			}
			// A placeholder card (see handleCreateGroup) has no identity beyond the
			// group; still count it as 0 members by not incrementing for it.
			if isPlaceholderGroupCard(ct) {
				continue
			}
			counts[key]++
		}
	}
	out := make([]groupInfo, 0, len(display))
	for key, name := range display {
		out = append(out, groupInfo{Name: name, Count: counts[key]})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return c.JSON(fiber.Map{"groups": out})
}

type groupInfo struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// placeholderGroupPrefix marks the FN of an empty placeholder contact created so
// a brand-new group is visible before any real contact is assigned to it.
const placeholderGroupPrefix = "__vulos-group__:"

func isPlaceholderGroupCard(ct models.Contact) bool {
	return strings.HasPrefix(ct.Name, placeholderGroupPrefix)
}

// handleCreateGroup makes a new group visible by writing a hidden placeholder
// card carrying only that CATEGORIES value. Assigning a real contact later and
// removing the placeholder keeps the group without the placeholder.
func (h *Handler) handleCreateGroup(c *fiber.Ctx) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	name := clampGroupName(body.Name)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "group name is required")
	}

	// Reject if the group already exists (case-insensitive) to keep names unique.
	contacts, err := h.listAllContacts(c, "", 0)
	if err == errContactsUnavailable {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not read groups")
	}
	for _, ct := range contacts {
		for _, g := range ct.Groups {
			if strings.EqualFold(strings.TrimSpace(g), name) {
				return fail(c, fiber.StatusConflict, "group already exists")
			}
		}
	}

	placeholder := models.Contact{
		Name:   placeholderGroupPrefix + name,
		Groups: []string{name},
	}
	if _, err := h.putContact(c, placeholder); err != nil {
		return fail(c, fiber.StatusBadGateway, "could not create group")
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"group": groupInfo{Name: name, Count: 0}})
}

// handleRenameGroup rewrites the CATEGORIES value on every card carrying the old
// name. Cards are rewritten one PUT at a time; a partial failure is surfaced.
func (h *Handler) handleRenameGroup(c *fiber.Ctx) error {
	old := strings.TrimSpace(c.Params("name"))
	if old == "" {
		return fail(c, fiber.StatusBadRequest, "group name required")
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid JSON body")
	}
	newName := clampGroupName(body.Name)
	if newName == "" {
		return fail(c, fiber.StatusBadRequest, "new group name is required")
	}

	changed, err := h.rewriteGroup(c, old, newName)
	if err == errContactsUnavailable {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not rename group")
	}
	return c.JSON(fiber.Map{"renamed": changed, "name": newName})
}

// handleDeleteGroup removes the group from every card that carries it (the cards
// themselves are kept; only the membership is dropped).
func (h *Handler) handleDeleteGroup(c *fiber.Ctx) error {
	name := strings.TrimSpace(c.Params("name"))
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "group name required")
	}
	changed, err := h.rewriteGroup(c, name, "") // empty target => remove
	if err == errContactsUnavailable {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not delete group")
	}
	return c.JSON(fiber.Map{"removed": changed})
}

// rewriteGroup walks the account's cards and, for each card carrying `old`,
// replaces it with `newName` (or drops it when newName is empty). A placeholder
// card left with no groups after removal is deleted so it does not linger. It
// returns the number of cards rewritten.
func (h *Handler) rewriteGroup(c *fiber.Ctx, old, newName string) (int, error) {
	contacts, err := h.listAllContacts(c, "", 0)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, ct := range contacts {
		next := make([]string, 0, len(ct.Groups))
		hit := false
		for _, g := range ct.Groups {
			if strings.EqualFold(strings.TrimSpace(g), old) {
				hit = true
				if newName != "" {
					next = append(next, newName)
				}
				continue
			}
			next = append(next, g)
		}
		if !hit {
			continue
		}
		ct.Groups = dedupeGroups(next)
		// A placeholder card whose only reason to exist was this group is deleted
		// once the group is gone from it.
		if isPlaceholderGroupCard(ct) && len(ct.Groups) == 0 {
			_ = h.deleteContact(c, ct.UID, ct.Path)
			changed++
			continue
		}
		if _, err := h.putContact(c, ct); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

func clampGroupName(s string) string {
	s = stripContactControl(s)
	if len(s) > maxGroupNameLen {
		s = s[:maxGroupNameLen]
	}
	return s
}

func dedupeGroups(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || seen[strings.ToLower(g)] {
			continue
		}
		seen[strings.ToLower(g)] = true
		out = append(out, g)
	}
	return out
}
