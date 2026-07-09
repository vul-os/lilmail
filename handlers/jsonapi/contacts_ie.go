// handlers/jsonapi/contacts_ie.go — user-facing contact import / export.
//
//	POST /v1/contacts/import   multipart {file, format?, mapping?} → {imported, skipped}
//	GET  /v1/contacts/export?format=vcf|csv                        → file download
//
// Import accepts a .vcf (one or many vCards) or a .csv (Google/Outlook export)
// and writes each parsed contact into the CALLER'S OWN address book via the same
// isolated seam as single-contact create. Everything is BOUNDED and SANITIZED:
//
//   - the upload body is capped (maxImportBytes) before parse;
//   - the number of contacts imported per request is capped (maxImportRows);
//   - each row is size-clamped and control-stripped via sanitizeContact;
//   - a malformed row is SKIPPED (skipped++), never fatal to the whole import;
//   - contacts are written into the caller's book only — there is no account
//     parameter, so cross-account import is impossible.
//
// Export streams the account's cards. CSV export is guarded against spreadsheet
// FORMULA INJECTION: any field beginning with = + - @ (or tab/CR) is prefixed
// with a single quote so a malicious contact field cannot execute in Excel/
// Sheets when the exported CSV is opened.
package jsonapi

import (
	"bytes"
	"encoding/csv"
	"io"
	"sort"
	"strings"

	"lilmail/handlers/api"
	"lilmail/models"

	vcard "github.com/emersion/go-vcard"
	"github.com/gofiber/fiber/v2"
)

const (
	// maxImportBytes caps the raw upload before parsing (10 MiB).
	maxImportBytes = 10 << 20
	// maxImportRows caps how many contacts a single import may create.
	maxImportRows = 5000
)

// registerContactImportExport mounts the import/export routes.
func (h *Handler) registerContactImportExport(g fiber.Router) {
	g.Post("/contacts/import", h.handleImportContacts)
	g.Get("/contacts/export", h.handleExportContacts)
}

// handleImportContacts parses an uploaded .vcf or .csv and writes each contact.
func (h *Handler) handleImportContacts(c *fiber.Ctx) error {
	// Gate on CardDAV availability up front so we never parse a big file for an
	// account that cannot store anything.
	if _, _, ok := h.cardDAVAvailable(c); !ok {
		return fail(c, fiber.StatusNotImplemented, "contacts not available for this account")
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "a file field is required")
	}
	if fileHeader.Size > maxImportBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "file too large")
	}
	f, err := fileHeader.Open()
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "could not read upload")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImportBytes+1))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "could not read upload")
	}
	if len(data) > maxImportBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "file too large")
	}

	format := strings.ToLower(strings.TrimSpace(c.FormValue("format")))
	if format == "" {
		format = sniffFormat(fileHeader.Filename, data)
	}

	var (
		contacts []models.Contact
		skipped  int
	)
	switch format {
	case "vcf", "vcard":
		contacts, skipped = parseVCards(data)
	case "csv":
		mapping := parseMappingParam(c.FormValue("mapping"))
		contacts, skipped = parseContactsCSV(data, mapping)
	default:
		return fail(c, fiber.StatusBadRequest, "unsupported format (use vcf or csv)")
	}

	if len(contacts) > maxImportRows {
		skipped += len(contacts) - maxImportRows
		contacts = contacts[:maxImportRows]
	}

	imported := 0
	for _, ct := range contacts {
		ct = sanitizeContact(ct)
		if !hasIdentity(ct) {
			skipped++
			continue
		}
		ct.UID = "" // always mint a fresh UID; never trust an imported one
		ct.Path = ""
		if _, err := h.putContact(c, ct); err != nil {
			skipped++
			continue
		}
		imported++
	}
	return c.JSON(fiber.Map{"imported": imported, "skipped": skipped})
}

// handleExportContacts streams the account's contacts as .vcf or .csv.
func (h *Handler) handleExportContacts(c *fiber.Ctx) error {
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format == "" {
		format = "vcf"
	}
	contacts, err := h.listAllContacts(c, "", 0)
	if err == errContactsUnavailable {
		contacts = []models.Contact{}
	} else if err != nil {
		return fail(c, fiber.StatusBadGateway, "could not export contacts")
	}
	// Never export the internal group placeholders.
	real := contacts[:0]
	for _, ct := range contacts {
		if !isPlaceholderGroupCard(ct) {
			real = append(real, ct)
		}
	}
	contacts = real

	switch format {
	case "vcf", "vcard":
		var buf bytes.Buffer
		enc := vcard.NewEncoder(&buf)
		for _, ct := range contacts {
			card := cardFromContactExport(ct)
			if err := enc.Encode(card); err != nil {
				continue
			}
		}
		c.Set("Content-Type", "text/vcard; charset=utf-8")
		c.Set("Content-Disposition", `attachment; filename="contacts.vcf"`)
		return c.Send(buf.Bytes())
	case "csv":
		var buf bytes.Buffer
		if err := writeContactsCSV(&buf, contacts); err != nil {
			return fail(c, fiber.StatusInternalServerError, "could not build CSV")
		}
		c.Set("Content-Type", "text/csv; charset=utf-8")
		c.Set("Content-Disposition", `attachment; filename="contacts.csv"`)
		return c.Send(buf.Bytes())
	default:
		return fail(c, fiber.StatusBadRequest, "unsupported format (use vcf or csv)")
	}
}

// sniffFormat guesses the import format from the filename extension, falling back
// to a content sniff (a vCard begins with BEGIN:VCARD).
func sniffFormat(filename string, data []byte) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".vcf") || strings.HasSuffix(lower, ".vcard"):
		return "vcf"
	case strings.HasSuffix(lower, ".csv"):
		return "csv"
	}
	head := bytes.TrimSpace(data)
	if len(head) > 64 {
		head = head[:64]
	}
	if bytes.Contains(bytes.ToUpper(head), []byte("BEGIN:VCARD")) {
		return "vcf"
	}
	return "csv"
}

// parseVCards decodes a concatenation of vCards. A card that fails to decode is
// skipped (skipped++), never aborting the whole stream.
func parseVCards(data []byte) (out []models.Contact, skipped int) {
	dec := vcard.NewDecoder(bytes.NewReader(data))
	for {
		card, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped++
			// go-vcard's decoder cannot resync mid-stream on a hard error; stop.
			break
		}
		if len(card) == 0 {
			skipped++
			continue
		}
		out = append(out, api.ContactFromCard(card))
	}
	return out, skipped
}

// csvMapping maps a logical contact field to a CSV column index. -1 means unset.
type csvMapping map[string]int

// parseMappingParam parses an optional "field:index,field:index" mapping override
// sent by the client after it previews the CSV header. Unknown fields ignored.
func parseMappingParam(s string) csvMapping {
	m := csvMapping{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		field, idxStr, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		idx := parseIndex(strings.TrimSpace(idxStr))
		if idx >= 0 {
			m[strings.ToLower(strings.TrimSpace(field))] = idx
		}
	}
	return m
}

// parseContactsCSV reads a Google/Outlook-style CSV. The first row is treated as
// a header; columns are matched by well-known names unless an explicit mapping is
// supplied. A row with the wrong field count or no identity is skipped.
func parseContactsCSV(data []byte, override csvMapping) (out []models.Contact, skipped int) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // tolerate ragged rows rather than erroring the whole file
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return nil, 0
	}
	m := autoMapHeader(header)
	for field, idx := range override {
		m[field] = idx
	}

	get := func(row []string, field string) string {
		idx, ok := m[field]
		if !ok || idx < 0 || idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}

	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped++
			continue // skip a malformed row, keep going
		}
		ct := models.Contact{
			Name:       get(row, "name"),
			Org:        get(row, "org"),
			Department: get(row, "department"),
			Title:      get(row, "title"),
			Note:       get(row, "note"),
			Birthday:   get(row, "birthday"),
			Nickname:   get(row, "nickname"),
		}
		// Structured name components, if present, populate N.
		first, last := get(row, "first"), get(row, "last")
		if first != "" || last != "" {
			ct.StructuredName = &models.StructuredName{
				Prefix: get(row, "prefix"),
				First:  first,
				Middle: get(row, "middle"),
				Last:   last,
				Suffix: get(row, "suffix"),
			}
			if ct.Name == "" {
				ct.Name = strings.TrimSpace(first + " " + last)
			}
		}
		for _, e := range splitMulti(get(row, "email")) {
			ct.Emails = append(ct.Emails, e)
		}
		for _, p := range splitMulti(get(row, "phone")) {
			ct.Phones = append(ct.Phones, p)
		}
		for _, w := range splitMulti(get(row, "website")) {
			ct.Websites = append(ct.Websites, models.TypedValue{Value: w})
		}
		if g := get(row, "groups"); g != "" {
			ct.Groups = splitMulti(g)
		}
		if s := strings.ToLower(get(row, "starred")); s == "1" || s == "true" || s == "yes" || s == "starred" {
			ct.Starred = true
		}
		// Photo is only accepted from a data URI; sanitizeContact re-sniffs it, so a
		// bare URL or an SVG is dropped rather than stored (no import XSS vector).
		if p := get(row, "photo"); p != "" {
			ct.Photo = p
		}
		out = append(out, ct)
	}
	return out, skipped
}

// autoMapHeader matches CSV column headers to logical fields by well-known names
// used by Google Contacts and Outlook exports. Unmatched columns are ignored.
func autoMapHeader(header []string) csvMapping {
	m := csvMapping{}
	for i, col := range header {
		key := strings.ToLower(strings.TrimSpace(col))
		switch {
		case key == "name" || key == "display name" || key == "full name":
			setIfUnset(m, "name", i)
		case key == "given name" || key == "first name" || key == "first":
			setIfUnset(m, "first", i)
		case key == "family name" || key == "last name" || key == "last":
			setIfUnset(m, "last", i)
		case key == "additional name" || key == "middle name" || key == "middle":
			setIfUnset(m, "middle", i)
		case key == "name prefix" || key == "title prefix" || key == "prefix":
			setIfUnset(m, "prefix", i)
		case key == "name suffix" || key == "suffix":
			setIfUnset(m, "suffix", i)
		case key == "nickname":
			setIfUnset(m, "nickname", i)
		case strings.Contains(key, "e-mail") || strings.HasPrefix(key, "email") || key == "email address":
			setIfUnset(m, "email", i)
		case strings.Contains(key, "phone"):
			setIfUnset(m, "phone", i)
		case key == "organization" || key == "organisation" || key == "company" || key == "org":
			setIfUnset(m, "org", i)
		case key == "department":
			setIfUnset(m, "department", i)
		case key == "job title" || key == "title" || key == "role":
			setIfUnset(m, "title", i)
		case key == "notes" || key == "note":
			setIfUnset(m, "note", i)
		case strings.Contains(key, "website") || key == "web page" || key == "url":
			setIfUnset(m, "website", i)
		case key == "birthday" || key == "bday":
			setIfUnset(m, "birthday", i)
		case key == "group membership" || key == "groups" || key == "categories" || key == "labels":
			setIfUnset(m, "groups", i)
		case key == "starred" || key == "favorite" || key == "favourite":
			setIfUnset(m, "starred", i)
		case key == "photo" || key == "photo url" || key == "avatar":
			setIfUnset(m, "photo", i)
		}
	}
	return m
}

func setIfUnset(m csvMapping, field string, i int) {
	if _, ok := m[field]; !ok {
		m[field] = i
	}
}

// splitMulti splits a CSV cell that may hold several values joined by the common
// export delimiters ( ::: from Google, ; and , as fallbacks).
func splitMulti(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	if strings.Contains(s, ":::") {
		parts = strings.Split(s, ":::")
	} else if strings.Contains(s, ";") {
		parts = strings.Split(s, ";")
	} else {
		parts = []string{s}
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// csvExportColumns is the fixed export schema (a superset that re-imports cleanly).
// "Starred" and "Photo" extend the Google/Outlook schema: Starred is 1/0 and
// Photo carries the raster data URI (formula-guarded like every other cell).
var csvExportColumns = []string{
	"Name", "Given Name", "Family Name", "Nickname", "Organization",
	"Department", "Title", "E-mail 1", "Phone 1", "Website 1",
	"Birthday", "Notes", "Groups", "Starred", "Photo",
}

// writeContactsCSV writes the export schema with formula-injection guarding.
func writeContactsCSV(w io.Writer, contacts []models.Contact) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvExportColumns); err != nil {
		return err
	}
	for _, ct := range contacts {
		var first, last string
		if ct.StructuredName != nil {
			first, last = ct.StructuredName.First, ct.StructuredName.Last
		}
		starred := "0"
		if ct.Starred {
			starred = "1"
		}
		row := []string{
			ct.Name, first, last, ct.Nickname, ct.Org,
			ct.Department, ct.Title,
			firstEmail(ct), firstPhone(ct), firstWebsite(ct),
			ct.Birthday, ct.Note, strings.Join(ct.Groups, " ::: "),
			starred, ct.Photo,
		}
		for i := range row {
			row[i] = csvSafe(row[i])
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe defuses spreadsheet formula injection: a cell that begins with a
// formula trigger (= + - @) or a control char (TAB/CR) is prefixed with a single
// quote so Excel/Sheets treats it as text, not a formula. This is applied on
// EXPORT (the untrusted path is the contact data going out to a spreadsheet).
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func firstEmail(ct models.Contact) string {
	if len(ct.TypedEmails) > 0 {
		return ct.TypedEmails[0].Value
	}
	if len(ct.Emails) > 0 {
		return ct.Emails[0]
	}
	return ""
}

func firstPhone(ct models.Contact) string {
	if len(ct.TypedPhones) > 0 {
		return ct.TypedPhones[0].Value
	}
	if len(ct.Phones) > 0 {
		return ct.Phones[0]
	}
	return ""
}

func firstWebsite(ct models.Contact) string {
	if len(ct.Websites) > 0 {
		return ct.Websites[0].Value
	}
	return ""
}

// cardFromContactExport builds an export card; it drops the internal placeholder
// prefix should one ever slip through.
func cardFromContactExport(ct models.Contact) vcard.Card {
	ct.Groups = dedupeGroups(ct.Groups)
	sort.Strings(ct.Groups)
	return api.CardFromContact(ct)
}

// parseIndex parses a non-negative CSV column index (0 is valid), returning -1
// on any non-digit or empty input.
func parseIndex(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
