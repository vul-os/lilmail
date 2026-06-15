// handlers/api/recipients_test.go — tests for the recent-recipients bbolt store.
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecentRecipientsStore_RecordAndSearch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	entries := []RecipientEntry{
		{Email: "alice@example.com", Name: "Alice"},
		{Email: "bob@example.com", Name: "Bob Smith"},
		{Email: "carol@other.org"},
	}
	if err := rs.Record(entries); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Search by email fragment.
	results, err := rs.Search("alice", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Email != "alice@example.com" {
		t.Errorf("Search alice: got %v", results)
	}

	// Search by name.
	results, err = rs.Search("smith", 10)
	if err != nil {
		t.Fatalf("Search smith: %v", err)
	}
	if len(results) != 1 || results[0].Email != "bob@example.com" {
		t.Errorf("Search smith: got %v", results)
	}

	// Empty query returns all.
	results, err = rs.Search("", 10)
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("Search empty: got %d results, want 3", len(results))
	}
}

func TestRecentRecipientsStore_CountIncrement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	entry := RecipientEntry{Email: "repeat@example.com", Name: "Repeat"}
	for i := 0; i < 5; i++ {
		if err := rs.Record([]RecipientEntry{entry}); err != nil {
			t.Fatalf("Record iteration %d: %v", i, err)
		}
	}

	results, err := rs.Search("repeat", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Count != 5 {
		t.Errorf("Count: got %d, want 5", results[0].Count)
	}
}

func TestRecentRecipientsStore_SortByCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	// Record "popular" twice and "rare" once.
	pop := RecipientEntry{Email: "popular@example.com", Name: "Popular"}
	rare := RecipientEntry{Email: "rare@example.com", Name: "Rare"}

	_ = rs.Record([]RecipientEntry{pop})
	_ = rs.Record([]RecipientEntry{pop})
	_ = rs.Record([]RecipientEntry{rare})

	results, err := rs.Search("", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	if results[0].Email != "popular@example.com" {
		t.Errorf("first result should be popular@example.com, got %q", results[0].Email)
	}
}

func TestRecentRecipientsStore_Limit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	var entries []RecipientEntry
	for i := 0; i < 20; i++ {
		entries = append(entries, RecipientEntry{
			Email: fmt.Sprintf("user%d@example.com", i),
		})
	}
	if err := rs.Record(entries); err != nil {
		t.Fatalf("Record: %v", err)
	}

	results, err := rs.Search("", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("limit 5: got %d results", len(results))
	}
}

func TestRecentRecipientsStore_NameUpdate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	// First record without name.
	_ = rs.Record([]RecipientEntry{{Email: "alice@example.com"}})
	// Second record with name — should update.
	_ = rs.Record([]RecipientEntry{{Email: "alice@example.com", Name: "Alice"}})

	results, err := rs.Search("alice", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if results[0].Name != "Alice" {
		t.Errorf("Name: got %q, want Alice", results[0].Name)
	}
}

func TestRecentRecipientsStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	// Write.
	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	_ = rs.Record([]RecipientEntry{{Email: "persist@example.com", Name: "Persist"}})
	rs.Close()

	// Verify file exists.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file missing: %v", err)
	}

	// Re-open and read.
	rs2, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer rs2.Close()

	results, err := rs2.Search("persist", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Email != "persist@example.com" {
		t.Errorf("persistence: got %v", results)
	}
}

func TestRecentRecipientsStore_LastUsedUpdated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "threads.db")

	rs, err := OpenRecipientsStore(dbPath)
	if err != nil {
		t.Fatalf("OpenRecipientsStore: %v", err)
	}
	defer rs.Close()

	before := time.Now().Add(-time.Second)
	_ = rs.Record([]RecipientEntry{{Email: "lu@example.com"}})
	after := time.Now().Add(time.Second)

	results, _ := rs.Search("lu", 10)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	lu := results[0].LastUsed
	if lu.Before(before) || lu.After(after) {
		t.Errorf("LastUsed %v not in expected range [%v, %v]", lu, before, after)
	}
}
