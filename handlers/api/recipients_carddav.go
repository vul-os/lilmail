// handlers/api/recipients_carddav.go — CardDAV address-book query for contacts.
//
// NOTE: end-to-end testing requires a live CardDAV server; the logic is
// architecturally correct and compile-tested but not integration-tested here.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	vcard "github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

// fetchCardDAVContacts queries a CardDAV server and returns contacts whose
// display name or email address contain the query string.
// Basic auth (username/password) is always used.
func fetchCardDAVContacts(serverURL, username, password, query string, limit int) ([]RecipientEntry, error) {
	if serverURL == "" {
		return nil, nil
	}
	// SSRF guard: validate the URL before dialing (see dav_url.go).
	if err := validateDAVURL(serverURL); err != nil {
		return nil, err
	}
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, username, password)
	return queryCardDAVContacts(hc, serverURL, query, limit)
}

// fetchCardDAVContactsBearer is the OAuth2/Bearer-token variant of
// fetchCardDAVContacts, used by the CP-brokered path: the access token is sent as
// an HTTP Authorization: Bearer header (reusing bearerHTTPClient from caldav.go)
// instead of basic auth.
func fetchCardDAVContactsBearer(serverURL, token, query string, limit int) ([]RecipientEntry, error) {
	if serverURL == "" || token == "" {
		return nil, nil
	}
	// SSRF / token-exfil guard: validate the (header-injected) URL BEFORE the
	// bearer token is attached to any request (see dav_url.go).
	if err := validateDAVURL(serverURL); err != nil {
		return nil, err
	}
	hc := &bearerHTTPClient{inner: http.DefaultClient, token: token}
	return queryCardDAVContacts(hc, serverURL, query, limit)
}

// queryCardDAVContacts performs address-book discovery and querying against an
// already-authenticated webdav.HTTPClient. It is the shared core of both the
// basic-auth and bearer-token entry points.
func queryCardDAVContacts(hc webdav.HTTPClient, serverURL, query string, limit int) ([]RecipientEntry, error) {
	client, err := carddav.NewClient(hc, serverURL)
	if err != nil {
		return nil, fmt.Errorf("carddav client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Discover principal → home set → address books.
	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		// Fall back to the configured URL itself as the principal.
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

	q := strings.ToLower(query)
	var results []RecipientEntry

	for _, book := range books {
		abQuery := &carddav.AddressBookQuery{
			DataRequest: carddav.AddressDataRequest{AllProp: true},
		}
		objects, err := client.QueryAddressBook(ctx, book.Path, abQuery)
		if err != nil {
			continue
		}
		for _, obj := range objects {
			name, emails := parseVCardEntry(obj.Card)
			for _, email := range emails {
				if q == "" ||
					strings.Contains(strings.ToLower(email), q) ||
					strings.Contains(strings.ToLower(name), q) {
					results = append(results, RecipientEntry{
						Email: email,
						Name:  name,
					})
					if limit > 0 && len(results) >= limit {
						return results, nil
					}
				}
			}
		}
	}

	return results, nil
}

// parseVCardEntry extracts the display name (FN) and email addresses from a
// go-vcard Card (which is map[string][]*Field).
func parseVCardEntry(card vcard.Card) (name string, emails []string) {
	// FN (formatted name).
	if fields, ok := card[vcard.FieldFormattedName]; ok && len(fields) > 0 {
		name = fields[0].Value
	}

	// EMAIL fields.
	for _, f := range card[vcard.FieldEmail] {
		if v := strings.TrimSpace(f.Value); v != "" {
			emails = append(emails, v)
		}
	}
	return name, emails
}
