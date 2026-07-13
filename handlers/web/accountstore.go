// handlers/web/accountstore.go
//
// AccountStore persists additional mail accounts for the multi-account feature.
// Each account entry contains the IMAP/SMTP credentials (AES-GCM encrypted with
// the same encryption key used for session credentials), a display label, and a
// colour tag for the UI badge.
//
// Storage: a single bbolt database (accounts.db, configurable) with one bucket
// per primary-account username.  The primary account itself is never stored here
// — it lives in the session.  Additional accounts are stored as JSON values keyed
// by their email address.
//
// Thread safety: bbolt handles concurrent reads and serialises writes internally,
// so AccountStore needs no lock of its own.
package web

import (
	"encoding/json"
	"fmt"
	"log"

	bolt "go.etcd.io/bbolt"
)

// AccountEntry holds one additional mail account.
type AccountEntry struct {
	// Email is the IMAP username / login address (also the bbolt key).
	Email string `json:"email"`
	// Label is the display name shown in the account switcher.
	Label string `json:"label"`
	// Color is an optional CSS colour string for the account badge.
	Color string `json:"color,omitempty"`
	// IMAPServer and SMTPServer override the global config values.
	IMAPServer string `json:"imap_server"`
	IMAPPort   int    `json:"imap_port"`
	SMTPServer string `json:"smtp_server"`
	SMTPPort   int    `json:"smtp_port"`
	// EncryptedPassword is AES-GCM ciphertext of the account password.
	// Encrypted with the same key as session credentials.
	EncryptedPassword string `json:"encrypted_password"`
}

// AccountStore manages per-primary-user account lists.
type AccountStore struct {
	db     *bolt.DB
	dbPath string
}

// OpenAccountStore opens (or creates) the accounts bbolt database at path.
func OpenAccountStore(path string) (*AccountStore, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("accountstore: open %s: %w", path, err)
	}
	s := &AccountStore{db: db, dbPath: path}
	return s, nil
}

// Close closes the underlying bbolt database.
func (s *AccountStore) Close() error {
	return s.db.Close()
}

// ensureBucket ensures the bucket for owner exists within tx.
func (s *AccountStore) ensureBucket(tx *bolt.Tx, owner string) (*bolt.Bucket, error) {
	b, err := tx.CreateBucketIfNotExists([]byte(owner))
	if err != nil {
		return nil, fmt.Errorf("accountstore: bucket for %s: %w", owner, err)
	}
	return b, nil
}

// Save upserts an account entry for the given primary-account owner.
func (s *AccountStore) Save(owner string, entry AccountEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("accountstore: marshal: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := s.ensureBucket(tx, owner)
		if err != nil {
			return err
		}
		return b.Put([]byte(entry.Email), raw)
	})
}

// Delete removes an account by email for the given owner.
func (s *AccountStore) Delete(owner, email string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(owner))
		if b == nil {
			return nil // nothing to delete
		}
		return b.Delete([]byte(email))
	})
}

// List returns all additional accounts for owner, sorted by email.
func (s *AccountStore) List(owner string) ([]AccountEntry, error) {
	var entries []AccountEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(owner))
		if b == nil {
			return nil // no accounts yet
		}
		return b.ForEach(func(_, v []byte) error {
			var e AccountEntry
			if err := json.Unmarshal(v, &e); err != nil {
				log.Printf("accountstore: unmarshal entry: %v", err)
				return nil // skip corrupt entry
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}
