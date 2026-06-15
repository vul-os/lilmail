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

	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, username, password)
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

