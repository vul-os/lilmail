// handlers/api/contacts_dav.go — CardDAV contact CRUD (list/create/update/delete).
//
// This extends the read-only address-book *search* in recipients_carddav.go to a
// full contacts surface: fetch complete cards, and PUT/DELETE vCards. It reuses
// the same SSRF-hardened HTTP client + address-book discovery, and exposes both
// basic-auth (standalone [carddav]) and bearer-token (CP-brokered) entry points,
// mirroring CardDAVContacts / CardDAVContactsBearer.
//
// NOTE: end-to-end testing requires a live CardDAV server; the logic is
// architecturally correct and compile-tested but not integration-tested here.
package api

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"lilmail/models"

	vcard "github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

// contactDAVTimeout bounds every CardDAV round-trip.
const contactDAVTimeout = 10 * time.Second

// davAuth captures how to authenticate a CardDAV request: either basic
// (username/password) or bearer (OAuth2 access token). Exactly one mode is used.
type davAuth struct {
	username string
	password string
	token    string // when set, bearer auth is used instead of basic
}

func (a davAuth) httpClient() webdav.HTTPClient {
	if a.token != "" {
		return &bearerHTTPClient{inner: safeDAVHTTPClient(), token: a.token}
	}
	return webdav.HTTPClientWithBasicAuth(safeDAVHTTPClient(), a.username, a.password)
}

// carddavClient builds an authenticated carddav.Client for serverURL after
// validating it against the SSRF guard.
func carddavClient(serverURL string, auth davAuth) (*carddav.Client, error) {
	if serverURL == "" {
		return nil, fmt.Errorf("carddav: URL is required")
	}
	if err := validateDAVURL(serverURL); err != nil {
		return nil, err
	}
	return carddav.NewClient(auth.httpClient(), serverURL)
}

// firstAddressBook discovers the first address book path for the principal,
// falling back to the server URL itself when discovery is unsupported.
func firstAddressBook(ctx context.Context, client *carddav.Client, serverURL string) (string, error) {
	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		principal = serverURL
	}
	homeSet, err := client.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return "", fmt.Errorf("carddav: find home set: %w", err)
	}
	books, err := client.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return "", fmt.Errorf("carddav: find address books: %w", err)
	}
	if len(books) == 0 {
		return "", fmt.Errorf("carddav: no address books found")
	}
	return books[0].Path, nil
}

// listContacts fetches full cards from all address books, filtered by query
// against name/email/org. Returns models.Contact with UID + object path so the
// caller can later edit or delete each entry.
func listContacts(serverURL string, auth davAuth, query string, limit int) ([]models.Contact, error) {
	client, err := carddavClient(serverURL, auth)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), contactDAVTimeout)
	defer cancel()

	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		principal = serverURL
	}
	homeSet, err := client.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("carddav: find home set: %w", err)
	}
	books, err := client.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("carddav: find address books: %w", err)
	}

	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]models.Contact, 0, 32)
	for _, book := range books {
		objects, err := client.QueryAddressBook(ctx, book.Path, &carddav.AddressBookQuery{
			DataRequest: carddav.AddressDataRequest{AllProp: true},
		})
		if err != nil {
			continue
		}
		for _, obj := range objects {
			ct := contactFromCard(obj.Card, obj.Path)
			if q != "" && !contactMatches(ct, q) {
				continue
			}
			out = append(out, ct)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// putContact creates or updates a contact. A blank UID mints a new one and PUTs a
// fresh object under the first address book; a set UID re-PUTs to ct.Path (edit)
// or a derived path. Returns the stored contact (with UID + Path populated).
func putContact(serverURL string, auth davAuth, ct models.Contact) (models.Contact, error) {
	client, err := carddavClient(serverURL, auth)
	if err != nil {
		return models.Contact{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), contactDAVTimeout)
	defer cancel()

	if ct.UID == "" {
		ct.UID = fmt.Sprintf("lilmail-%d", time.Now().UnixNano())
	}
	objPath := ct.Path
	if objPath == "" {
		book, err := firstAddressBook(ctx, client, serverURL)
		if err != nil {
			return models.Contact{}, err
		}
		objPath = path.Join(book, ct.UID+".vcf")
	}

	card := cardFromContact(ct)
	if _, err := client.PutAddressObject(ctx, objPath, card); err != nil {
		return models.Contact{}, fmt.Errorf("carddav: put address object: %w", err)
	}
	ct.Path = objPath
	return ct, nil
}

// deleteContact removes a contact. When objPath is empty it is resolved by
// scanning the address books for a card with a matching UID.
func deleteContact(serverURL string, auth davAuth, uid, objPath string) error {
	client, err := carddavClient(serverURL, auth)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), contactDAVTimeout)
	defer cancel()

	if objPath == "" {
		if uid == "" {
			return fmt.Errorf("carddav: delete requires a UID or path")
		}
		found, err := findContactPath(ctx, client, serverURL, uid)
		if err != nil {
			return err
		}
		objPath = found
	}
	// carddav.Client embeds *webdav.Client, which exposes RemoveAll (HTTP DELETE).
	if err := client.RemoveAll(ctx, objPath); err != nil {
		return fmt.Errorf("carddav: delete address object: %w", err)
	}
	return nil
}

// findContactPath locates the object path of the card with the given UID.
func findContactPath(ctx context.Context, client *carddav.Client, serverURL, uid string) (string, error) {
	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		principal = serverURL
	}
	homeSet, err := client.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return "", fmt.Errorf("carddav: find home set: %w", err)
	}
	books, err := client.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return "", fmt.Errorf("carddav: find address books: %w", err)
	}
	for _, book := range books {
		objects, err := client.QueryAddressBook(ctx, book.Path, &carddav.AddressBookQuery{
			DataRequest: carddav.AddressDataRequest{AllProp: true},
		})
		if err != nil {
			continue
		}
		for _, obj := range objects {
			if cardUID(obj.Card) == uid {
				return obj.Path, nil
			}
		}
	}
	return "", fmt.Errorf("carddav: contact %q not found", uid)
}

// ── vCard <-> models.Contact ──────────────────────────────────────────────
//
// The rich field mapping (structured N, TYPE labels, ADR, BDAY, URL, IMPP,
// CATEGORIES, ORG dept) lives in contact_vcard.go so it can be unit-tested in
// isolation. cardUID and contactMatches remain here alongside the DAV plumbing.

func cardUID(card vcard.Card) string {
	if f := card.Get(vcard.FieldUID); f != nil {
		return f.Value
	}
	return ""
}

func contactMatches(ct models.Contact, q string) bool {
	if strings.Contains(strings.ToLower(ct.Name), q) ||
		strings.Contains(strings.ToLower(ct.Org), q) {
		return true
	}
	for _, e := range ct.Emails {
		if strings.Contains(strings.ToLower(e), q) {
			return true
		}
	}
	return false
}

// ── Public entry points (basic + bearer), mirroring CardDAVContacts* ───────

// ContactsList returns full cards via basic auth. Returns an empty slice when the
// URL is unset. Errors are surfaced so the API can report a real failure.
func ContactsList(serverURL, username, password, query string, limit int) ([]models.Contact, error) {
	if serverURL == "" {
		return []models.Contact{}, nil
	}
	return listContacts(serverURL, davAuth{username: username, password: password}, query, limit)
}

// ContactsListBearer is the OAuth2/Bearer variant of ContactsList.
func ContactsListBearer(serverURL, token, query string, limit int) ([]models.Contact, error) {
	if serverURL == "" || token == "" {
		return []models.Contact{}, nil
	}
	return listContacts(serverURL, davAuth{token: token}, query, limit)
}

// ContactPut creates/updates a contact via basic auth.
func ContactPut(serverURL, username, password string, ct models.Contact) (models.Contact, error) {
	return putContact(serverURL, davAuth{username: username, password: password}, ct)
}

// ContactPutBearer is the OAuth2/Bearer variant of ContactPut.
func ContactPutBearer(serverURL, token string, ct models.Contact) (models.Contact, error) {
	return putContact(serverURL, davAuth{token: token}, ct)
}

// ContactDelete removes a contact via basic auth.
func ContactDelete(serverURL, username, password, uid, objPath string) error {
	return deleteContact(serverURL, davAuth{username: username, password: password}, uid, objPath)
}

// ContactDeleteBearer is the OAuth2/Bearer variant of ContactDelete.
func ContactDeleteBearer(serverURL, token, uid, objPath string) error {
	return deleteContact(serverURL, davAuth{token: token}, uid, objPath)
}
