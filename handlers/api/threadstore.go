// handlers/api/threadstore.go — bbolt-backed message-header cache for JWZ threading.
//
// The store persists per-folder header metadata (UID → msgMeta JSON) in a
// bbolt database so that threads can be rebuilt over a larger window than the
// current 50-message IMAP page.
//
// Usage:
//
//	store, err := OpenThreadStore(boltPath)
//	defer store.Close()
//	threads, err := store.BuildThreads(folder, emails)
//
// If the DB is missing, corrupt, or otherwise unusable BuildThreads falls back
// to threading just the in-memory emails.
package api

import (
	"encoding/json"
	"fmt"
	"lilmail/models"
	"log"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// msgMeta is the slim record persisted per message in the bolt store.
type msgMeta struct {
	MessageID  string    `json:"mid,omitempty"`
	InReplyTo  string    `json:"irt,omitempty"`
	References []string  `json:"refs,omitempty"`
	Subject    string    `json:"subj,omitempty"`
	From       string    `json:"from,omitempty"`
	FromName   string    `json:"fromName,omitempty"`
	Date       time.Time `json:"date"`
	Preview    string    `json:"preview,omitempty"`
	Flags      []string  `json:"flags,omitempty"`
}

// ThreadStore is a long-lived bbolt handle shared across requests for a single
// user.  bbolt allows multiple concurrent readers but only one writer at a
// time; the embedded mutex serialises the writes while reads use bbolt's own
// concurrent-read support.
type ThreadStore struct {
	db   *bolt.DB
	mu   sync.Mutex // guards writes (bolt.Update calls)
	path string
}

// OpenThreadStore opens (or creates) the bbolt database at boltPath and
// returns a ThreadStore that can be reused for many requests.  The caller must
// call Close() when done (typically at server shutdown or user logout).
func OpenThreadStore(boltPath string) (*ThreadStore, error) {
	db, err := bolt.Open(boltPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("threadstore: open %s: %w", boltPath, err)
	}
	return &ThreadStore{db: db, path: boltPath}, nil
}

// Close releases the bbolt file handle.
func (s *ThreadStore) Close() error {
	return s.db.Close()
}

// BuildThreads upserts emails into the bolt store, loads all cached headers for
// the folder, runs ThreadMessages over the union, and returns threads.
// folder is the IMAP folder name used as the bucket name.
//
// On any DB error the function logs the error and falls back to threading
// only the supplied in-memory emails.
func (s *ThreadStore) BuildThreads(folder string, emails []models.Email) ([]models.Thread, error) {
	bucket := []byte(folder)

	// ---- upsert current emails ------------------------------------------
	s.mu.Lock()
	upsertErr := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return fmt.Errorf("create bucket: %w", err)
		}
		for i := range emails {
			e := &emails[i]
			uid := e.ID
			if uid == "" {
				continue
			}
			meta := msgMeta{
				MessageID:  e.MessageID,
				InReplyTo:  e.InReplyTo,
				References: e.References,
				Subject:    e.Subject,
				From:       e.From,
				FromName:   e.FromName,
				Date:       e.Date,
				Preview:    e.Preview,
				Flags:      e.Flags,
			}
			raw, err := json.Marshal(meta)
			if err != nil {
				continue
			}
			_ = b.Put([]byte(uid), raw)
		}
		return nil
	})
	s.mu.Unlock()

	if upsertErr != nil {
		log.Printf("threadstore: upsert %s/%s: %v", s.path, folder, upsertErr)
		// Still attempt to read whatever was previously stored.
	}

	// ---- load all cached headers for this folder ------------------------
	// Build a set of UIDs already in the in-memory slice so we use the
	// richer in-memory version (with body, attachments etc.) when available.
	inMem := make(map[string]int, len(emails)) // uid → index in emails
	for i := range emails {
		inMem[emails[i].ID] = i
	}

	var union []models.Email
	// Seed with in-memory emails first (they have the most fields populated).
	union = append(union, emails...)

	readErr := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			uid := string(k)
			if _, ok := inMem[uid]; ok {
				return nil // already included from in-memory slice
			}
			var meta msgMeta
			if err := json.Unmarshal(v, &meta); err != nil {
				return nil // skip corrupt records
			}
			union = append(union, models.Email{
				ID:         uid,
				MessageID:  meta.MessageID,
				InReplyTo:  meta.InReplyTo,
				References: meta.References,
				Subject:    meta.Subject,
				From:       meta.From,
				FromName:   meta.FromName,
				Date:       meta.Date,
				Preview:    meta.Preview,
				Flags:      meta.Flags,
			})
			return nil
		})
	})
	if readErr != nil {
		log.Printf("threadstore: read %s/%s: %v — using in-memory only", s.path, folder, readErr)
		return ThreadMessages(emails), nil
	}

	return ThreadMessages(union), nil
}

// BuildThreads is a package-level convenience wrapper that opens a fresh bolt
// handle per call.  It is retained for backwards compatibility with callers
// that have not yet migrated to a shared ThreadStore.
//
// Deprecated: prefer OpenThreadStore + (*ThreadStore).BuildThreads to avoid
// opening the single-writer bbolt file on every request.
func BuildThreads(boltPath, folder string, emails []models.Email) ([]models.Thread, error) {
	db, err := bolt.Open(boltPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Printf("threadstore: open %s: %v — falling back to in-memory threading", boltPath, err)
		return ThreadMessages(emails), nil
	}
	defer db.Close()

	ts := &ThreadStore{db: db, path: boltPath}
	return ts.BuildThreads(folder, emails)
}
