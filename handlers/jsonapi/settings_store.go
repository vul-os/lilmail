// handlers/jsonapi/settings_store.go — durable per-account settings for the
// Gmail-parity surfaces: vacation responder, signatures, and send-as identities.
//
// STORAGE: one JSON blob per (owner, kind) in the storage.KV seam (bbolt by
// default, Postgres when configured — the same seam scheduled-send/snooze use).
// The key is "<owner>|<kind>" so every record is OWNED by exactly one account
// (the key prefix): a read/write keyed on another owner's prefix simply cannot
// address this owner's record. owner is the authenticated identity (fromEmail),
// which is the authz boundary — there is no code path by which one user reads
// another user's settings.
//
// These records hold NO secrets (a signature/vacation body/alias address is not
// a credential), so unlike the account store they are stored as plain JSON. The
// per-user isolation still holds structurally via the key prefix.
package jsonapi

import (
	"encoding/json"
	"errors"

	"lilmail/storage"
)

// settingsNS is the KV namespace holding per-account settings blobs.
const settingsNS = "mail_settings"

// Settings "kinds" — the suffix of the composite key.
const (
	kindVacation   = "vacation"
	kindSignatures = "signatures"
	kindIdentities = "identities"
)

// vacationConfig is the out-of-office responder configuration. Enforcement in the
// IMAP-broker model is documented in settings.go (handleGetVacation); this is the
// authoritative stored config the client edits and the delivery path consumes.
type vacationConfig struct {
	Enabled bool   `json:"enabled"`
	Subject string `json:"subject"`
	Body    string `json:"body"` // sanitized HTML (sanitizeSnippetHTML)
	// StartAt/EndAt bound when the responder is active, RFC3339. Empty => unbounded
	// on that side. The delivery path (or a live Sieve) checks the window at send.
	StartAt string `json:"startAt,omitempty"`
	EndAt   string `json:"endAt,omitempty"`
	// RespondOnlyToContacts, when true, limits auto-replies to known contacts —
	// an anti-backscatter measure on top of the built-in loop protection.
	RespondOnlyToContacts bool `json:"respondOnlyToContacts,omitempty"`
}

// signature is one named HTML signature. Default marks the account-wide default;
// a per-identity default is expressed via identity.DefaultSignatureID.
type signature struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	HTML    string `json:"html"` // sanitized HTML (sanitizeSnippetHTML)
	Default bool   `json:"default,omitempty"`
}

// identity is a send-as alias the account may send from. Address is the From the
// compose window offers; DefaultSignatureID links a signature to this identity.
type identity struct {
	Address            string `json:"address"`
	Name               string `json:"name,omitempty"`
	DefaultSignatureID string `json:"defaultSignatureId,omitempty"`
	// IsPrimary marks the account's own mailbox address (always present, not
	// user-removable) so the client can render it distinctly.
	IsPrimary bool `json:"isPrimary,omitempty"`
}

// settingsStore reads/writes the per-account settings blobs via the KV seam.
type settingsStore struct {
	kv storage.KV
}

func newSettingsStore(kv storage.KV) *settingsStore {
	return &settingsStore{kv: kv}
}

func settingsKey(owner, kind string) string { return owner + "|" + kind }

// get loads the JSON blob for (owner, kind) into v. A missing record is NOT an
// error — v is left at its zero value so callers get sensible empty defaults.
func (s *settingsStore) get(owner, kind string, v any) error {
	if s == nil || s.kv == nil {
		return errors.New("settings storage unavailable")
	}
	b, err := s.kv.Get(settingsNS, settingsKey(owner, kind))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// put stores v as the JSON blob for (owner, kind).
func (s *settingsStore) put(owner, kind string, v any) error {
	if s == nil || s.kv == nil {
		return errors.New("settings storage unavailable")
	}
	if owner == "" {
		return errors.New("settings: owner required")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.kv.Set(settingsNS, settingsKey(owner, kind), b)
}
