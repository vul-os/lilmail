// handlers/web/pushstore.go
//
// PushStore persists Web Push subscription objects in a per-user bbolt bucket.
// Each subscription is keyed by its endpoint URL (truncated to 512 chars as
// the bbolt key) and stored as JSON.
//
// Thread safety: the bbolt database handle is safe for concurrent use; we rely
// on bbolt's own locking.  PushStore itself adds a sync.Mutex for the map that
// caches open database handles.
package web

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"

	bolt "go.etcd.io/bbolt"
)

const pushBucket = "push_subscriptions"

// PushSubscription mirrors the browser's PushSubscription JSON serialisation.
// https://w3c.github.io/push-api/#dom-pushsubscription-tojson
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// subscriptionKey returns a stable bbolt key for a subscription.
// We use the endpoint URL directly (truncated to 512 bytes so we don't bust
// bbolt's 65535-byte key limit).
func subscriptionKey(endpoint string) []byte {
	if len(endpoint) > 512 {
		return []byte(endpoint[:512])
	}
	return []byte(endpoint)
}

// PushStore manages push subscriptions stored in per-user bbolt databases.
type PushStore struct {
	mu       sync.Mutex
	cacheDir string
	openDBs  map[string]*bolt.DB
}

// NewPushStore creates a PushStore that will keep bbolt files inside cacheDir.
func NewPushStore(cacheDir string) *PushStore {
	return &PushStore{
		cacheDir: cacheDir,
		openDBs:  make(map[string]*bolt.DB),
	}
}

// db returns (or opens) the bbolt handle for username.
func (s *PushStore) db(username string) (*bolt.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if db, ok := s.openDBs[username]; ok {
		return db, nil
	}
	path := filepath.Join(s.cacheDir, username, "push.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("pushstore: open %s: %w", path, err)
	}
	// Ensure bucket exists.
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(pushBucket))
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("pushstore: create bucket: %w", err)
	}
	s.openDBs[username] = db
	return db, nil
}

// Save upserts a subscription for the given user.
func (s *PushStore) Save(username string, sub PushSubscription) error {
	db, err := s.db(username)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("pushstore: marshal: %w", err)
	}
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(pushBucket))
		return b.Put(subscriptionKey(sub.Endpoint), raw)
	})
}

// Delete removes a subscription by endpoint.
func (s *PushStore) Delete(username, endpoint string) error {
	db, err := s.db(username)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(pushBucket))
		return b.Delete(subscriptionKey(endpoint))
	})
}

// All returns all subscriptions for username.
func (s *PushStore) All(username string) ([]PushSubscription, error) {
	db, err := s.db(username)
	if err != nil {
		return nil, err
	}
	var subs []PushSubscription
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(pushBucket))
		return b.ForEach(func(_, v []byte) error {
			var sub PushSubscription
			if err := json.Unmarshal(v, &sub); err != nil {
				log.Printf("pushstore: unmarshal subscription: %v", err)
				return nil // skip corrupt entry
			}
			subs = append(subs, sub)
			return nil
		})
	})
	return subs, err
}
