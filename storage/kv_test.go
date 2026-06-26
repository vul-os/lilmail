package storage

import (
	"bytes"
	"path/filepath"
	"testing"

	"lilmail/config"
)

// Exercises the KV contract against the default bolt backend. The Postgres
// backend satisfies the same interface and is covered by integration tests
// where a database is available.
func TestBoltKVRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	kv, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if _, err := kv.Get("threads", "missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	if err := kv.Set("threads", "a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Set("threads", "ab", []byte("2")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Set("recipients", "a", []byte("x")); err != nil {
		t.Fatal(err)
	}

	v, err := kv.Get("threads", "a")
	if err != nil || !bytes.Equal(v, []byte("1")) {
		t.Fatalf("get a: %q %v", v, err)
	}

	// List honours namespace isolation and prefix.
	all, err := kv.List("threads", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list threads: %v len=%d", err, len(all))
	}
	pre, err := kv.List("threads", "ab")
	if err != nil || len(pre) != 1 {
		t.Fatalf("list prefix ab: %v len=%d", err, len(pre))
	}

	if err := kv.Delete("threads", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get("threads", "a"); err != ErrNotFound {
		t.Fatalf("after delete want ErrNotFound, got %v", err)
	}
}

// Open() must default to bolt when no backend is configured, preserving the
// standalone single-binary behaviour.
func TestOpenDefaultsToBolt(t *testing.T) {
	cfg := &config.Config{} // empty: Storage.Backend == ""
	kv, err := Open(cfg, filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	if err := kv.Set("ns", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
}
