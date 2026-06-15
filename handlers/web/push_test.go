// handlers/web/push_test.go — unit tests for VAPID key management,
// PushStore, and push payload building.
package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ── VAPID key tests ─────────────────────────────────────────────────────────

func TestLoadOrGenerateVAPIDKeys_Generate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")

	keys, err := LoadOrGenerateVAPIDKeys(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if keys.Public == "" || keys.Private == "" {
		t.Fatal("generated keys are empty")
	}

	// File must exist after generation.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key file not created: %v", err)
	}

	// File should be 0600.
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Errorf("key file mode %o, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrGenerateVAPIDKeys_Load(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")

	// Generate once.
	first, err := LoadOrGenerateVAPIDKeys(path)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}

	// Load again — should return the same keys.
	second, err := LoadOrGenerateVAPIDKeys(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if first.Public != second.Public || first.Private != second.Private {
		t.Error("keys differ between generate and load")
	}
}

func TestLoadOrGenerateVAPIDKeys_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")

	// Write garbage.
	if err := os.WriteFile(path, []byte("not-json{{{"), 0600); err != nil {
		t.Fatal(err)
	}

	// Should regenerate rather than fail.
	keys, err := LoadOrGenerateVAPIDKeys(path)
	if err != nil {
		t.Fatalf("should regenerate on corrupt file: %v", err)
	}
	if keys.Public == "" {
		t.Fatal("regenerated keys are empty")
	}
}

// ── PushStore tests ──────────────────────────────────────────────────────────

func makePushStore(t *testing.T) (*PushStore, string) {
	t.Helper()
	dir := t.TempDir()
	// PushStore expects per-user subdirectories; create one for "alice".
	if err := os.MkdirAll(filepath.Join(dir, "alice"), 0700); err != nil {
		t.Fatal(err)
	}
	return NewPushStore(dir), dir
}

func TestPushStore_SaveAndAll(t *testing.T) {
	store, _ := makePushStore(t)

	sub := PushSubscription{Endpoint: "https://push.example.com/abc"}
	sub.Keys.P256DH = "p256dhvalue"
	sub.Keys.Auth = "authvalue"

	if err := store.Save("alice", sub); err != nil {
		t.Fatalf("Save: %v", err)
	}

	subs, err := store.All("alice")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	if subs[0].Endpoint != sub.Endpoint {
		t.Errorf("endpoint: got %q, want %q", subs[0].Endpoint, sub.Endpoint)
	}
}

func TestPushStore_Delete(t *testing.T) {
	store, _ := makePushStore(t)

	sub := PushSubscription{Endpoint: "https://push.example.com/del"}
	sub.Keys.P256DH = "key"
	sub.Keys.Auth = "auth"

	_ = store.Save("alice", sub)

	if err := store.Delete("alice", sub.Endpoint); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	subs, _ := store.All("alice")
	if len(subs) != 0 {
		t.Errorf("expected 0 subscriptions after delete, got %d", len(subs))
	}
}

func TestPushStore_MultipleSubscriptions(t *testing.T) {
	store, _ := makePushStore(t)

	for i := 0; i < 3; i++ {
		sub := PushSubscription{Endpoint: "https://push.example.com/sub" + string(rune('0'+i))}
		sub.Keys.P256DH = "key"
		sub.Keys.Auth = "auth"
		_ = store.Save("alice", sub)
	}

	subs, err := store.All("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 3 {
		t.Errorf("expected 3 subscriptions, got %d", len(subs))
	}
}

func TestPushStore_Upsert(t *testing.T) {
	store, _ := makePushStore(t)

	sub := PushSubscription{Endpoint: "https://push.example.com/upsert"}
	sub.Keys.P256DH = "old-key"
	sub.Keys.Auth = "auth"
	_ = store.Save("alice", sub)

	sub.Keys.P256DH = "new-key"
	_ = store.Save("alice", sub)

	subs, _ := store.All("alice")
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription after upsert, got %d", len(subs))
	}
	if subs[0].Keys.P256DH != "new-key" {
		t.Errorf("P256DH not updated: got %q", subs[0].Keys.P256DH)
	}
}

func TestPushStore_IsolatedByUser(t *testing.T) {
	dir := t.TempDir()
	// Create subdirs for both users.
	_ = os.MkdirAll(filepath.Join(dir, "alice"), 0700)
	_ = os.MkdirAll(filepath.Join(dir, "bob"), 0700)
	store := NewPushStore(dir)

	subA := PushSubscription{Endpoint: "https://push.example.com/alice"}
	subA.Keys.P256DH = "ka"
	subA.Keys.Auth = "a"

	subB := PushSubscription{Endpoint: "https://push.example.com/bob"}
	subB.Keys.P256DH = "kb"
	subB.Keys.Auth = "b"

	_ = store.Save("alice", subA)
	_ = store.Save("bob", subB)

	aliceSubs, _ := store.All("alice")
	bobSubs, _ := store.All("bob")

	if len(aliceSubs) != 1 || aliceSubs[0].Endpoint != subA.Endpoint {
		t.Errorf("alice subs wrong: %v", aliceSubs)
	}
	if len(bobSubs) != 1 || bobSubs[0].Endpoint != subB.Endpoint {
		t.Errorf("bob subs wrong: %v", bobSubs)
	}
}

// ── Push payload tests ───────────────────────────────────────────────────────

func TestBuildPushPayload(t *testing.T) {
	ev := MailEvent{From: "Alice <alice@example.com>", Subject: "Hello world"}
	payload := buildPushPayload(ev)

	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if m["from"] != ev.From {
		t.Errorf("from: got %v, want %q", m["from"], ev.From)
	}
	if m["subject"] != ev.Subject {
		t.Errorf("subject: got %v, want %q", m["subject"], ev.Subject)
	}
	if m["tag"] != "newmail" {
		t.Errorf("tag: got %v, want 'newmail'", m["tag"])
	}
	// Must be under 4 KB (RFC 8030).
	if len(payload) > 4096 {
		t.Errorf("payload %d bytes exceeds 4096 limit", len(payload))
	}
}

func TestBuildPushPayload_SpecialChars(t *testing.T) {
	ev := MailEvent{
		From:    `Bob <b@x.com> "tricky" & <b>`,
		Subject: "Subject with\nnewlines\ttabs",
	}
	payload := buildPushPayload(ev)
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload with special chars not valid JSON: %v", err)
	}
	if m["from"] != ev.From {
		t.Errorf("from not preserved: got %v", m["from"])
	}
}
