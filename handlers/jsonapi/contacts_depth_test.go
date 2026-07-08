package jsonapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"

	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// fakeBook is an in-memory CardDAV address book keyed by the account's CardDAV
// URL, so a test can prove per-account isolation: a write against one URL must
// never surface under another.
type fakeBook struct {
	mu    sync.Mutex
	books map[string][]models.Contact // cardDAVURL -> contacts
	seq   int
}

func newFakeBook() *fakeBook { return &fakeBook{books: map[string][]models.Contact{}} }

func (b *fakeBook) list(url, query string) []models.Contact {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	out := []models.Contact{}
	for _, ct := range b.books[url] {
		if q != "" && !strings.Contains(strings.ToLower(ct.Name+" "+strings.Join(ct.Emails, " ")), q) {
			continue
		}
		out = append(out, ct)
	}
	return out
}

func (b *fakeBook) put(url string, ct models.Contact) models.Contact {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ct.UID == "" {
		b.seq++
		ct.UID = fmt.Sprintf("uid-%d", b.seq)
	}
	if ct.Path == "" {
		ct.Path = "/ab/" + ct.UID + ".vcf"
	}
	// Re-project so the round-trip mirrors CardDAV (flat emails populated, etc.).
	list := b.books[url]
	for i, existing := range list {
		if existing.UID == ct.UID {
			list[i] = ct
			b.books[url] = list
			return ct
		}
	}
	b.books[url] = append(list, ct)
	return ct
}

func (b *fakeBook) del(url, uid string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.books[url]
	out := list[:0]
	for _, ct := range list {
		if ct.UID != uid {
			out = append(out, ct)
		}
	}
	b.books[url] = out
}

// installFakeBook wires the CardDAV seams to an in-memory book for the test and
// restores them afterwards.
func installFakeBook(t *testing.T, b *fakeBook) {
	t.Helper()
	l, p, d := brokerContactsList, brokerContactPut, brokerContactDelete
	brokerContactsList = func(spec brokerSpec, query string, limit int) ([]models.Contact, error) {
		return b.list(spec.CardDAVURL, query), nil
	}
	brokerContactPut = func(spec brokerSpec, ct models.Contact) (models.Contact, error) {
		return b.put(spec.CardDAVURL, ct), nil
	}
	brokerContactDelete = func(spec brokerSpec, uid, objPath string) error {
		b.del(spec.CardDAVURL, uid)
		return nil
	}
	t.Cleanup(func() { brokerContactsList, brokerContactPut, brokerContactDelete = l, p, d })
}

// bookReq builds a brokered request targeting the given CardDAV URL, reusing the
// package-shared brokeredReq(method, path, body, extra) helper.
func bookReq(method, path, cardDAVURL string, body io.Reader) *http.Request {
	extra := map[string]string{}
	if cardDAVURL != "" {
		extra[hdrMailCardDAVURL] = cardDAVURL
	}
	return brokeredReq(method, path, body, extra)
}

const bookA = "https://dav.example.com/a/"
const bookB = "https://dav.example.com/b/"

func newContactsApp(t *testing.T) *fiber.App {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

// ── Groups CRUD + assign + filter ─────────────────────────────────────────

func TestGroupCRUDAndFilter(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	// Create a group → visible via list with count 0.
	do(t, app, brokeredReqJSON("POST", "/v1/contacts/groups", bookA, `{"name":"Team"}`), fiber.StatusCreated)
	if got := listGroups(t, app, bookA); len(got) != 1 || got[0].Name != "Team" || got[0].Count != 0 {
		t.Fatalf("after create: %+v", got)
	}

	// Assign two contacts to the group via normal create (groups field).
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"Alice","emails":["a@x.com"],"groups":["Team"]}`), fiber.StatusCreated)
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"Bob","emails":["b@x.com"],"groups":["Team"]}`), fiber.StatusCreated)
	if got := listGroups(t, app, bookA); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("after assign: %+v", got)
	}

	// Filter the cards list by group.
	cards := listCards(t, app, bookA, "?group=Team")
	if len(cards) != 2 {
		t.Fatalf("group filter returned %d, want 2", len(cards))
	}
	if len(listCards(t, app, bookA, "?group=Nope")) != 0 {
		t.Fatal("unknown group should return no cards")
	}

	// Rename the group → both cards updated.
	do(t, app, brokeredReqJSON("PATCH", "/v1/contacts/groups/Team", bookA, `{"name":"Squad"}`), fiber.StatusOK)
	if got := listGroups(t, app, bookA); len(got) != 1 || got[0].Name != "Squad" || got[0].Count != 2 {
		t.Fatalf("after rename: %+v", got)
	}
	if len(listCards(t, app, bookA, "?group=Squad")) != 2 {
		t.Fatal("cards not under renamed group")
	}

	// Delete the group → membership removed from cards (cards survive).
	do(t, app, bookReq("DELETE", "/v1/contacts/groups/Squad", bookA, nil), fiber.StatusOK)
	if got := listGroups(t, app, bookA); len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
	if len(listCards(t, app, bookA, "")) != 2 {
		t.Fatal("contacts should survive group deletion")
	}
}

// ── Import: vCard ─────────────────────────────────────────────────────────

func TestImportVCard(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Grace Hopper\r\nEMAIL;TYPE=work:grace@navy.mil\r\nTEL;TYPE=cell:+1 555 0000\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Katherine Johnson\r\nEMAIL:kj@nasa.gov\r\nEND:VCARD\r\n"
	res := importFile(t, app, bookA, "contacts.vcf", "", vcf, "")
	if res.Imported != 2 || res.Skipped != 0 {
		t.Fatalf("vcard import: %+v", res)
	}
	cards := listCards(t, app, bookA, "")
	if len(cards) != 2 {
		t.Fatalf("stored %d cards", len(cards))
	}
}

// A malformed vCard mid-file is skipped, not fatal.
func TestImportVCardMalformedSkipped(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	// One valid card, then garbage. The decoder stops on the hard error but the
	// valid card before it is kept, and the import is not a 4xx/5xx.
	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Valid One\r\nEMAIL:v@x.com\r\nEND:VCARD\r\nthis is not a vcard at all"
	res := importFile(t, app, bookA, "c.vcf", "", vcf, "")
	if res.Imported < 1 {
		t.Fatalf("valid card should import despite trailing garbage: %+v", res)
	}
}

// ── Import: CSV with column mapping + malformed-skip ──────────────────────

func TestImportCSVWithHeaderMapping(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	csv := "Name,Given Name,Family Name,E-mail 1 Value,Phone 1 Value,Organization\r\n" +
		"Ada Lovelace,Ada,Lovelace,ada@x.com,+1 555,Engines\r\n" +
		"Bob,Bob,,bob@y.com,,\r\n"
	res := importFile(t, app, bookA, "google.csv", "", csv, "")
	if res.Imported != 2 {
		t.Fatalf("csv import: %+v", res)
	}
	cards := listCards(t, app, bookA, "")
	var ada *models.Contact
	for i := range cards {
		if strings.Contains(cards[i].Name, "Ada") {
			ada = &cards[i]
		}
	}
	if ada == nil {
		t.Fatal("Ada not imported")
	}
	if len(ada.Emails) == 0 || ada.Emails[0] != "ada@x.com" {
		t.Errorf("email mapping failed: %+v", ada.Emails)
	}
	if ada.Org != "Engines" {
		t.Errorf("org mapping failed: %q", ada.Org)
	}
}

// A CSV row with no name and no email is skipped.
func TestImportCSVSkipsEmptyRow(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	csv := "Name,E-mail 1 Value\r\nReal,real@x.com\r\n,\r\n"
	res := importFile(t, app, bookA, "c.csv", "", csv, "")
	if res.Imported != 1 {
		t.Fatalf("expected 1 imported (empty row skipped): %+v", res)
	}
}

// An explicit mapping override targets columns by index even without headers
// matching well-known names.
func TestImportCSVExplicitMapping(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	csv := "col0,col1\r\nWidget Person,wp@x.com\r\n"
	res := importFile(t, app, bookA, "weird.csv", "csv", csv, "name:0,email:1")
	if res.Imported != 1 {
		t.Fatalf("explicit mapping import: %+v", res)
	}
	cards := listCards(t, app, bookA, "")
	if len(cards) != 1 || cards[0].Name != "Widget Person" || len(cards[0].Emails) == 0 {
		t.Fatalf("mapping override did not apply: %+v", cards)
	}
}

// ── Export: CSV formula-injection guard + vCard ───────────────────────────

func TestExportCSVFormulaInjectionGuarded(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	// A malicious contact whose name is a spreadsheet formula.
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"=HYPERLINK(\"http://evil\")","emails":["evil@x.com"]}`), fiber.StatusCreated)

	body := exportBody(t, app, bookA, "csv")
	// The dangerous cell must be neutralised with a leading quote.
	if !strings.Contains(body, "'=HYPERLINK") {
		t.Fatalf("formula not neutralised in CSV export:\n%s", body)
	}
	if strings.Contains(body, ",=HYPERLINK") || strings.HasPrefix(body, "=HYPERLINK") {
		t.Fatalf("raw formula leaked into CSV:\n%s", body)
	}
}

func TestExportVCard(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"Ada","emails":["ada@x.com"],"typedPhones":[{"value":"+1 555","type":"mobile"}]}`), fiber.StatusCreated)
	body := exportBody(t, app, bookA, "vcf")
	if !strings.Contains(body, "BEGIN:VCARD") || !strings.Contains(body, "ada@x.com") {
		t.Fatalf("vcard export missing content:\n%s", body)
	}
}

// ── Per-account isolation ─────────────────────────────────────────────────

// An import into book A must not appear in book B, and export from B must be
// empty — proving contacts are written to the caller's book only.
func TestImportExportPerAccountIsolation(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	importFile(t, app, bookA, "a.vcf", "", "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Only A\r\nEMAIL:a@x.com\r\nEND:VCARD\r\n", "")

	if len(listCards(t, app, bookA, "")) != 1 {
		t.Fatal("book A should have 1 contact")
	}
	if got := listCards(t, app, bookB, ""); len(got) != 0 {
		t.Fatalf("book B leaked A's contacts: %+v", got)
	}
	if b := exportBody(t, app, bookB, "vcf"); strings.Contains(b, "Only A") {
		t.Fatalf("export from B leaked A's contact:\n%s", b)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

type importResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

func do(t *testing.T, app *fiber.App, req *http.Request, wantStatus int) []byte {
	t.Helper()
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: got %d want %d: %s", req.Method, req.URL.Path, resp.StatusCode, wantStatus, body)
	}
	return body
}

func brokeredReqJSON(method, path, url, jsonBody string) *http.Request {
	// bookReq -> brokeredReq sets Content-Type: application/json when body != nil.
	return bookReq(method, path, url, strings.NewReader(jsonBody))
}

func listGroups(t *testing.T, app *fiber.App, url string) []groupInfo {
	t.Helper()
	body := do(t, app, bookReq("GET", "/v1/contacts/groups", url, nil), fiber.StatusOK)
	var out struct {
		Groups []groupInfo `json:"groups"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("groups json: %s", body)
	}
	return out.Groups
}

func listCards(t *testing.T, app *fiber.App, url, qs string) []models.Contact {
	t.Helper()
	body := do(t, app, bookReq("GET", "/v1/contacts/cards"+qs, url, nil), fiber.StatusOK)
	var out struct {
		Contacts []models.Contact `json:"contacts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("cards json: %s", body)
	}
	return out.Contacts
}

func importFile(t *testing.T, app *fiber.App, url, filename, format, content, mapping string) importResult {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", filename)
	fw.Write([]byte(content))
	if format != "" {
		w.WriteField("format", format)
	}
	if mapping != "" {
		w.WriteField("mapping", mapping)
	}
	w.Close()

	req := bookReq("POST", "/v1/contacts/import", url, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	body := do(t, app, req, fiber.StatusOK)
	var res importResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("import json: %s", body)
	}
	return res
}

func exportBody(t *testing.T, app *fiber.App, url, format string) string {
	t.Helper()
	body := do(t, app, bookReq("GET", "/v1/contacts/export?format="+format, url, nil), fiber.StatusOK)
	return string(body)
}
