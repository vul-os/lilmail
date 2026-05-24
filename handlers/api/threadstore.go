// handlers/api/threadstore.go — bbolt-backed message-header cache for JWZ threading.
//
// The store persists per-folder header metadata (UID → msgMeta JSON) in a
// bbolt database so that threads can be rebuilt over a larger window than the
// current 50-message IMAP page.
//
// Usage:
//
//	threads, err := BuildThreads(boltPath, folder, emails)
//
// If the DB is missing, corrupt, or otherwise unusable the function falls back
// to threading just the in-memory emails.
package api

import (
	"encoding/json"
	"fmt"
	"lilmail/models"
	"log"
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

// BuildThreads upserts emails into the bolt store, loads all cached headers for
// the folder, runs ThreadMessages over the union, and returns threads.
//
// boltPath is the full filesystem path to the .db file (e.g.
// filepath.Join(userCacheFolder, "threads.db")).  folder is the IMAP folder
// name used as the bucket name.
//
// On any DB error the function logs the error and falls back to threading
// only the supplied in-memory emails.
func BuildThreads(boltPath, folder string, emails []models.Email) ([]models.Thread, error) {
	db, err := bolt.Open(boltPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Printf("threadstore: open %s: %v — falling back to in-memory threading", boltPath, err)
		return ThreadMessages(emails), nil
	}
	defer db.Close()

	bucket := []byte(folder)

	// ---- upsert current emails ------------------------------------------
	if upsertErr := db.Update(func(tx *bolt.Tx) error {
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
	}); upsertErr != nil {
		log.Printf("threadstore: upsert %s/%s: %v", boltPath, folder, upsertErr)
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

	readErr := db.View(func(tx *bolt.Tx) error {
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
		log.Printf("threadstore: read %s/%s: %v — using in-memory only", boltPath, folder, readErr)
		return ThreadMessages(emails), nil
	}

	return ThreadMessages(union), nil
}
