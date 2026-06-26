// Package storage defines lilmail's durable key-value seam and its backends.
//
// The seam is one small interface (KV) with two implementations:
//
//   - bolt (DEFAULT): an embedded bbolt file, zero external services. This is
//     the standalone single-binary path and what ships out of the box.
//   - postgres (OPTIONAL): a shared SQL store, opt-in via [storage] config.
//     Use it when multiple instances must share state, or when another Vulos
//     service (Workspace, the cloud control plane) needs to read the same data.
//
// Existing direct-bbolt users (handlers/api.ThreadStore, push/recipient stores)
// keep working unchanged; this seam is for new and migratable callers that want
// a backend-agnostic store. Open() picks the backend from config so callers
// never branch on it themselves.
package storage

import (
	"errors"
	"fmt"

	"lilmail/config"
)

// ErrNotFound is returned by Get when a namespace/key pair does not exist.
var ErrNotFound = errors.New("storage: not found")

// KV is a namespaced byte-blob store. A namespace is an isolated keyspace
// (a bbolt bucket / a logical partition of the SQL table). Implementations
// must be safe for concurrent use.
type KV interface {
	// Get returns the value for key in ns, or ErrNotFound.
	Get(ns, key string) ([]byte, error)
	// Set stores val for key in ns, creating the namespace if needed.
	Set(ns, key string, val []byte) error
	// Delete removes key from ns; deleting a missing key is not an error.
	Delete(ns, key string) error
	// List returns all key→value pairs in ns whose key has the given prefix
	// (pass "" for the whole namespace).
	List(ns, prefix string) (map[string][]byte, error)
	// Close releases the backend's resources.
	Close() error
}

// Open constructs the KV backend selected by cfg.Storage. boltPath is used only
// by the bolt backend (ignored for postgres). An empty/unknown backend defaults
// to bolt so a missing [storage] section keeps the standalone behaviour.
func Open(cfg *config.Config, boltPath string) (KV, error) {
	switch cfg.Storage.Backend {
	case "", "bolt":
		return OpenBolt(boltPath)
	case "postgres":
		if cfg.Storage.PostgresDSN == "" {
			return nil, fmt.Errorf("storage: backend=postgres requires storage.postgres_dsn")
		}
		return OpenPostgres(cfg.Storage.PostgresDSN)
	default:
		return nil, fmt.Errorf("storage: unknown backend %q (want bolt|postgres)", cfg.Storage.Backend)
	}
}
