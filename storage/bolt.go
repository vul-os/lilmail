package storage

import (
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// boltKV is the default embedded backend: one bbolt file, one bucket per
// namespace. It needs no external services and keeps lilmail a single binary.
type boltKV struct {
	db *bolt.DB
}

// OpenBolt opens (creating if needed) a bbolt database at path.
func OpenBolt(path string) (KV, error) {
	if path == "" {
		return nil, fmt.Errorf("storage: bolt path is empty")
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: open bolt %s: %w", path, err)
	}
	return &boltKV{db: db}, nil
}

func (b *boltKV) Get(ns, key string) ([]byte, error) {
	var out []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte(ns))
		if bk == nil {
			return ErrNotFound
		}
		v := bk.Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		out = append([]byte(nil), v...) // copy: bbolt bytes are tx-scoped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *boltKV) Set(ns, key string, val []byte) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		bk, err := tx.CreateBucketIfNotExists([]byte(ns))
		if err != nil {
			return err
		}
		return bk.Put([]byte(key), val)
	})
}

func (b *boltKV) Delete(ns, key string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte(ns))
		if bk == nil {
			return nil
		}
		return bk.Delete([]byte(key))
	})
}

func (b *boltKV) List(ns, prefix string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := b.db.View(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte(ns))
		if bk == nil {
			return nil // empty namespace → empty result
		}
		return bk.ForEach(func(k, v []byte) error {
			if prefix == "" || strings.HasPrefix(string(k), prefix) {
				out[string(k)] = append([]byte(nil), v...)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *boltKV) Close() error { return b.db.Close() }
