package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileStorageDirectoryPermissions verifies that NewFileStorage creates the
// sessions directory with mode 0700 (owner-only), not the world-readable 0755.
func TestFileStorageDirectoryPermissions(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sessions")

	_, err := NewFileStorage(dir)
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat sessions dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("sessions directory: want mode 0700, got %04o", got)
	}
}

// TestFileStorageFilePermissions verifies that session files are written with
// mode 0600 (owner read/write only), not the world-readable 0644.
func TestFileStorageFilePermissions(t *testing.T) {
	dir := t.TempDir()

	fs, err := NewFileStorage(dir)
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}

	if err := fs.Set("testkey", []byte(`{"hello":"world"}`), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "testkey.session"))
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("session file: want mode 0600, got %04o", got)
	}
}

// TestFileStorageRoundtrip is a basic sanity check that Set/Get work.
func TestFileStorageRoundtrip(t *testing.T) {
	dir := t.TempDir()

	fs, err := NewFileStorage(dir)
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}

	val := []byte("sessiondata")
	if err := fs.Set("key1", val, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := fs.Get("key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(val) {
		t.Errorf("Get: want %q, got %q", val, got)
	}
}
