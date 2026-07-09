package jsonapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

var pngHeader = append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("pixels")...)

func uploadPhoto(t *testing.T, app *fiber.App, url, uid, filename string, content []byte) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", filename)
	fw.Write(content)
	w.Close()
	req := bookReq("POST", "/v1/contacts/"+uid+"/photo", url, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	body := new(bytes.Buffer)
	body.ReadFrom(resp.Body)
	return resp, body.String()
}

func firstCardUID(t *testing.T, app *fiber.App, url string) string {
	t.Helper()
	cards := listCards(t, app, url, "")
	if len(cards) == 0 {
		t.Fatal("no cards")
	}
	return cards[0].UID
}

// A raster upload attaches and round-trips through the cards list.
func TestPhotoUploadRoundTrip(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"Ada","emails":["ada@x.com"]}`), fiber.StatusCreated)
	uid := firstCardUID(t, app, bookA)

	resp, body := uploadPhoto(t, app, bookA, uid, "avatar.png", pngHeader)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("upload status %d: %s", resp.StatusCode, body)
	}
	cards := listCards(t, app, bookA, "")
	if len(cards) != 1 || !strings.HasPrefix(cards[0].Photo, "data:image/png;base64,") {
		t.Fatalf("photo not attached: %+v", cards)
	}

	// Delete the photo.
	do(t, app, bookReq("DELETE", "/v1/contacts/"+uid+"/photo", bookA, nil), fiber.StatusOK)
	if p := listCards(t, app, bookA, "")[0].Photo; p != "" {
		t.Fatalf("photo not removed: %q", p)
	}
}

// SVG / HTML uploads are rejected (415) — no stored XSS.
func TestPhotoUploadRejectsSVG(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"M","emails":["m@x.com"]}`), fiber.StatusCreated)
	uid := firstCardUID(t, app, bookA)

	resp, _ := uploadPhoto(t, app, bookA, uid, "x.svg", []byte(`<svg onload="alert(1)"></svg>`))
	if resp.StatusCode != fiber.StatusUnsupportedMediaType {
		t.Fatalf("SVG upload status = %d, want 415", resp.StatusCode)
	}
	if p := listCards(t, app, bookA, "")[0].Photo; p != "" {
		t.Fatalf("SVG must not be stored, got %q", p)
	}
}

// An oversize upload is rejected and never stored.
func TestPhotoUploadSizeCap(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"Big","emails":["b@x.com"]}`), fiber.StatusCreated)
	uid := firstCardUID(t, app, bookA)

	big := append(append([]byte{}, pngHeader...), bytes.Repeat([]byte{0}, (2<<20)+16)...)
	resp, _ := uploadPhoto(t, app, bookA, uid, "big.png", big)
	if resp.StatusCode == fiber.StatusOK {
		t.Fatal("oversize photo must be rejected")
	}
	if p := listCards(t, app, bookA, "")[0].Photo; p != "" {
		t.Fatalf("oversize photo must not be stored, got len %d", len(p))
	}
}

// Uploading a photo for a UID that lives in ANOTHER account's book must 404 — the
// upload can only touch the caller's own book.
func TestPhotoUploadPerAccountIsolation(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA, `{"name":"A","emails":["a@x.com"]}`), fiber.StatusCreated)
	uidA := firstCardUID(t, app, bookA)

	// Account B tries to set a photo on A's contact UID.
	resp, _ := uploadPhoto(t, app, bookB, uidA, "a.png", pngHeader)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("cross-account photo upload status = %d, want 404", resp.StatusCode)
	}
	// A's contact is untouched.
	if p := listCards(t, app, bookA, "")[0].Photo; p != "" {
		t.Fatalf("A's photo must be unchanged, got %q", p)
	}
}

// A photo set via the JSON create body is sanitized: a valid raster is kept, an
// SVG data URI is dropped.
func TestPhotoViaJSONSanitized(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngHeader)
	svg := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>"))

	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"Good","emails":["g@x.com"],"photo":"`+png+`"}`), fiber.StatusCreated)
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"Bad","emails":["b@x.com"],"photo":"`+svg+`"}`), fiber.StatusCreated)

	cards := listCards(t, app, bookA, "")
	for _, ct := range cards {
		switch ct.Name {
		case "Good":
			if !strings.HasPrefix(ct.Photo, "data:image/png;base64,") {
				t.Errorf("good photo dropped: %q", ct.Photo)
			}
		case "Bad":
			if ct.Photo != "" {
				t.Errorf("SVG photo must be dropped via JSON: %q", ct.Photo)
			}
		}
	}
}

// ── Starred ───────────────────────────────────────────────────────────────

// Starred round-trips via CRUD and is not surfaced as a group.
func TestStarredCRUDAndNotAGroup(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"Fav","emails":["f@x.com"],"starred":true,"groups":["Team"]}`), fiber.StatusCreated)

	cards := listCards(t, app, bookA, "")
	if len(cards) != 1 || !cards[0].Starred {
		t.Fatalf("starred not round-tripped: %+v", cards)
	}
	// The reserved category must not appear as a group.
	groups := listGroups(t, app, bookA)
	for _, g := range groups {
		if strings.Contains(strings.ToLower(g.Name), "starred") {
			t.Fatalf("starred category leaked into group list: %+v", groups)
		}
	}
	if len(groups) != 1 || groups[0].Name != "Team" {
		t.Fatalf("groups = %+v, want [Team]", groups)
	}
}

// ── CSV export columns include Starred + Photo, still formula-guarded ──────

func TestCSVExportPhotoAndStarredColumns(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngHeader)
	do(t, app, brokeredReqJSON("POST", "/v1/contacts", bookA,
		`{"name":"Ada","emails":["ada@x.com"],"starred":true,"photo":"`+png+`"}`), fiber.StatusCreated)

	body := exportBody(t, app, bookA, "csv")
	header := strings.SplitN(body, "\n", 2)[0]
	if !strings.Contains(header, "Starred") || !strings.Contains(header, "Photo") {
		t.Fatalf("CSV header missing Starred/Photo: %q", header)
	}
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("photo data URI not in CSV export:\n%s", body)
	}
	// The starred column carries 1.
	if !strings.Contains(body, ",1,data:image/png") && !strings.Contains(body, ",1,\"data:image/png") {
		// tolerate csv quoting; just require a starred=1 followed by the photo cell
		if !strings.Contains(body, "1,data:image") {
			t.Fatalf("starred column not 1 next to photo:\n%s", body)
		}
	}
}

// ── Frequently contacted ──────────────────────────────────────────────────

func TestFrequentContactsSurfacing(t *testing.T) {
	book := newFakeBook()
	installFakeBook(t, book)
	app := newContactsApp(t)

	// With no send history / no cache folder configured, the endpoint degrades to
	// an empty list rather than erroring.
	body := do(t, app, bookReq("GET", "/v1/contacts/frequent", bookA, nil), fiber.StatusOK)
	var out struct {
		Contacts []frequentContact `json:"contacts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("frequent json: %s", body)
	}
	if out.Contacts == nil {
		t.Fatal("contacts should be [] not null")
	}
	if len(out.Contacts) != 0 {
		t.Fatalf("expected empty frequent list, got %+v", out.Contacts)
	}
	_ = models.Contact{}
}

// With a real per-account recipients store populated (as the send path would), the
// endpoint surfaces the most-contacted addresses in count-desc order.
func TestFrequentContactsFromStore(t *testing.T) {
	t.Setenv(brokerEnvSecret, "s3cr3t")
	store := session.New()
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	// Pre-populate the store at the exact path the endpoint reads (username =
	// "user@gmail.com" from brokeredHeaders()).
	dbPath := filepath.Join(cfg.Cache.Folder, api.SanitizeUsername("user@gmail.com"), "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	rs, err := api.OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// bob sent to 3x, alice 1x.
	rs.Record([]api.RecipientEntry{{Email: "bob@x.com", Name: "Bob"}})
	rs.Record([]api.RecipientEntry{{Email: "bob@x.com", Name: "Bob"}})
	rs.Record([]api.RecipientEntry{{Email: "bob@x.com", Name: "Bob"}})
	rs.Record([]api.RecipientEntry{{Email: "alice@x.com", Name: "Alice"}})
	rs.Close()

	body := do(t, app, bookReq("GET", "/v1/contacts/frequent", bookA, nil), fiber.StatusOK)
	var out struct {
		Contacts []frequentContact `json:"contacts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("frequent json: %s", body)
	}
	if len(out.Contacts) != 2 {
		t.Fatalf("expected 2 frequent contacts, got %+v", out.Contacts)
	}
	if out.Contacts[0].Email != "bob@x.com" || out.Contacts[0].Count != 3 {
		t.Fatalf("most-contacted should be bob x3: %+v", out.Contacts[0])
	}
}
