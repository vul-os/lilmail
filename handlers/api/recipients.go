// handlers/api/recipients.go — recent-recipients store + CardDAV contacts.
//
// RecentRecipientsStore persists (in bbolt) the list of addresses the user
// has sent mail to. The autocomplete endpoint merges those with any CardDAV
// contacts returned by the configured CardDAV server.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	recipientsBucket = "recent_recipients"
	maxRecipients    = 500 // cap stored addresses
)

// RecipientEntry is one stored contact / recent recipient.
type RecipientEntry struct {
	Email    string    `json:"email"`
	Name     string    `json:"name,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Count    int       `json:"count"` // number of times sent to
}

// RecentRecipientsStore is a bbolt-backed store of recently-sent-to addresses,
// keyed by the user's bbolt path (same file as the thread store).
type RecentRecipientsStore struct {
	db *bolt.DB
}

// OpenRecipientsStore opens the bbolt database at the given path (shared with
// the thread store) and returns a store.
func OpenRecipientsStore(boltPath string) (*RecentRecipientsStore, error) {
	db, err := bolt.Open(boltPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("recipients: open %s: %w", boltPath, err)
	}
	return &RecentRecipientsStore{db: db}, nil
}

// Close releases the bbolt file handle.
func (s *RecentRecipientsStore) Close() error {
	return s.db.Close()
}

// Record upserts a list of recipient addresses as recently-used. name is
// optional (the display name from the To/CC/BCC field).
func (s *RecentRecipientsStore) Record(entries []RecipientEntry) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(recipientsBucket))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Email == "" {
				continue
			}
			key := []byte(strings.ToLower(entry.Email))
			// Load existing record to merge counts.
			var existing RecipientEntry
			if raw := b.Get(key); raw != nil {
				_ = json.Unmarshal(raw, &existing)
			}
			if existing.Email == "" {
				existing.Email = entry.Email
			}
			if entry.Name != "" {
				existing.Name = entry.Name
			}
			existing.LastUsed = time.Now()
			existing.Count++
			raw, _ := json.Marshal(existing)
			_ = b.Put(key, raw)
		}
		return nil
	})
}

// Search returns up to limit RecipientEntry values whose email or name contain
// query (case-insensitive). Results are sorted by (count desc, last_used desc).
func (s *RecentRecipientsStore) Search(query string, limit int) ([]RecipientEntry, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	var results []RecipientEntry

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(recipientsBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var entry RecipientEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil
			}
			if q == "" ||
				strings.Contains(strings.ToLower(entry.Email), q) ||
				strings.Contains(strings.ToLower(entry.Name), q) {
				results = append(results, entry)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// Sort: count descending, then last_used descending.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Count != results[j].Count {
			return results[i].Count > results[j].Count
		}
		return results[i].LastUsed.After(results[j].LastUsed)
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// ParseAddressField parses a comma-separated address field like
//
//	"Alice <alice@example.com>, bob@example.com"
//
// into RecipientEntry values with email and optional name.
func ParseAddressField(field string) []RecipientEntry {
	if field == "" {
		return nil
	}
	var out []RecipientEntry
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Try "Name <addr>" form.
		if lt := strings.LastIndex(part, "<"); lt >= 0 {
			name := strings.TrimSpace(part[:lt])
			addr := strings.TrimSpace(strings.TrimSuffix(part[lt+1:], ">"))
			if addr != "" {
				out = append(out, RecipientEntry{Email: addr, Name: name})
			}
		} else {
			out = append(out, RecipientEntry{Email: part})
		}
	}
	return out
}

// CardDAVContacts returns contacts from the CardDAV server whose display name
// or email contains the query string. Returns nil without error when CardDAV is
// not configured or the query returns no results. The function does not log
// verbose errors — a CardDAV failure degrades gracefully to recent-only results.
func CardDAVContacts(cardDavURL, username, password, query string, limit int) []RecipientEntry {
	if cardDavURL == "" {
		return nil
	}
	// Import the CardDAV client lazily via the configured URL. We use go-webdav
	// directly since we only need address-book query (carddav.Client.QueryAddressBook).
	// The implementation is in recipients_carddav.go to keep this file focused.
	results, err := fetchCardDAVContacts(cardDavURL, username, password, query, limit)
	if err != nil {
		log.Printf("recipients: CardDAV search %q: %v", query, err)
		return nil
	}
	return results
}

// CardDAVContactsBearer is the OAuth2/Bearer-token variant of CardDAVContacts,
// used by the CP-brokered path: it authenticates with the supplied access token
// as an HTTP Bearer header instead of basic auth. Returns nil (degrading
// gracefully) when the URL or token is empty, or on any CardDAV failure.
func CardDAVContactsBearer(cardDavURL, token, query string, limit int) []RecipientEntry {
	if cardDavURL == "" || token == "" {
		return nil
	}
	results, err := fetchCardDAVContactsBearer(cardDavURL, token, query, limit)
	if err != nil {
		log.Printf("recipients: CardDAV (bearer) search %q: %v", query, err)
		return nil
	}
	return results
}
