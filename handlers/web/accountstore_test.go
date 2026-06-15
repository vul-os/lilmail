// handlers/web/accountstore_test.go — unit tests for AccountStore.
package web

import (
	"path/filepath"
	"testing"
)

func openTestAccountStore(t *testing.T) (*AccountStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.db")
	s, err := OpenAccountStore(path)
	if err != nil {
		t.Fatalf("OpenAccountStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestAccountStore_SaveAndList(t *testing.T) {
	s, _ := openTestAccountStore(t)

	entry := AccountEntry{
		Email:             "work@company.com",
		Label:             "Work",
		Color:             "#4285F4",
		IMAPServer:        "imap.company.com",
		IMAPPort:          993,
		SMTPServer:        "smtp.company.com",
		SMTPPort:          587,
		EncryptedPassword: "encryptedblob",
	}

	if err := s.Save("alice@personal.com", entry); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := s.List("alice@personal.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.Email != entry.Email {
		t.Errorf("email: got %q, want %q", got.Email, entry.Email)
	}
	if got.Label != entry.Label {
		t.Errorf("label: got %q, want %q", got.Label, entry.Label)
	}
	if got.IMAPServer != entry.IMAPServer {
		t.Errorf("imap_server: got %q, want %q", got.IMAPServer, entry.IMAPServer)
	}
	if got.EncryptedPassword != entry.EncryptedPassword {
		t.Errorf("encrypted_password not preserved")
	}
}

func TestAccountStore_Delete(t *testing.T) {
	s, _ := openTestAccountStore(t)

	entry := AccountEntry{Email: "extra@example.com", Label: "Extra"}
	_ = s.Save("owner@example.com", entry)

	if err := s.Delete("owner@example.com", entry.Email); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, _ := s.List("owner@example.com")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after delete, got %d", len(entries))
	}
}

func TestAccountStore_DeleteNonExistent(t *testing.T) {
	s, _ := openTestAccountStore(t)
	// Should not error when entry doesn't exist.
	if err := s.Delete("owner@example.com", "nobody@example.com"); err != nil {
		t.Errorf("delete non-existent: unexpected error: %v", err)
	}
}

func TestAccountStore_Upsert(t *testing.T) {
	s, _ := openTestAccountStore(t)

	entry := AccountEntry{Email: "work@example.com", Label: "Old label"}
	_ = s.Save("me@example.com", entry)

	entry.Label = "New label"
	_ = s.Save("me@example.com", entry)

	entries, _ := s.List("me@example.com")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after upsert, got %d", len(entries))
	}
	if entries[0].Label != "New label" {
		t.Errorf("label not updated: got %q", entries[0].Label)
	}
}

func TestAccountStore_IsolatedByOwner(t *testing.T) {
	s, _ := openTestAccountStore(t)

	_ = s.Save("alice@example.com", AccountEntry{Email: "alice-extra@example.com", Label: "A"})
	_ = s.Save("bob@example.com", AccountEntry{Email: "bob-extra@example.com", Label: "B"})

	aliceEntries, _ := s.List("alice@example.com")
	bobEntries, _ := s.List("bob@example.com")

	if len(aliceEntries) != 1 || aliceEntries[0].Email != "alice-extra@example.com" {
		t.Errorf("alice entries wrong: %v", aliceEntries)
	}
	if len(bobEntries) != 1 || bobEntries[0].Email != "bob-extra@example.com" {
		t.Errorf("bob entries wrong: %v", bobEntries)
	}
}

func TestAccountStore_EmptyList(t *testing.T) {
	s, _ := openTestAccountStore(t)

	entries, err := s.List("nobody@example.com")
	if err != nil {
		t.Fatalf("List for unknown owner: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty list, got %d entries", len(entries))
	}
}

func TestAccountStore_MultipleEntries(t *testing.T) {
	s, _ := openTestAccountStore(t)

	for _, email := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		_ = s.Save("owner@x.com", AccountEntry{Email: email, Label: email})
	}

	entries, _ := s.List("owner@x.com")
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
}

func TestAccountStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.db")

	// Write.
	s1, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Save("owner@x.com", AccountEntry{Email: "persist@x.com", Label: "Persist"})
	s1.Close()

	// Re-open and verify.
	s2, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	entries, _ := s2.List("owner@x.com")
	if len(entries) != 1 || entries[0].Email != "persist@x.com" {
		t.Errorf("persistence failed: got %v", entries)
	}
}
