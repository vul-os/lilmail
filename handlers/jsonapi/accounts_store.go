// handlers/jsonapi/accounts_store.go — durable per-user store of CONNECTED
// accounts for the /v1 multi-account / unified-inbox surface.
//
// PORTED FROM handlers/web/accountstore.go (the legacy HTMX multi-account impl),
// re-homed onto the storage.KV seam so it shares one backend with the rest of the
// /v1 durable state (bbolt standalone, Postgres when configured) instead of a
// separate accounts.db bbolt file. The credential-at-rest model is IDENTICAL:
// each account's IMAP/SMTP password is AES-GCM encrypted with config.Encryption.Key
// (api.EncryptJSON) and NEVER returned to the client in cleartext.
//
// ISOLATION: the key is "<owner>|<accountEmail>" where owner is the authenticated
// primary identity (fromEmail). List uses the "<owner>|" prefix, and Get/Delete
// are keyed by (owner, accountEmail) — so a user can only ever enumerate, read,
// or remove THEIR OWN connected accounts. Another user's accountEmail keyed under
// the wrong owner prefix simply misses (404 no-leak). This mirrors the
// scheduled-send store's per-account isolation exactly.
package jsonapi

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"lilmail/storage"
)

// connAccountsNS is the KV namespace holding connected-account records.
const connAccountsNS = "connected_accounts"

// maxConnectedAccounts bounds how many extra accounts a single user may connect,
// so the durable store + the concurrent unified fetch cannot be made unbounded.
const maxConnectedAccounts = 25

// connectedAccount is one additional mailbox a user has connected. It carries the
// IMAP/SMTP coordinates + the ENCRYPTED password; the plaintext password is only
// ever transiently in memory during add-validation or a unified fetch.
type connectedAccount struct {
	Email      string `json:"email"` // IMAP login / mailbox address (the key suffix)
	Label      string `json:"label"`
	Color      string `json:"color,omitempty"`
	IMAPServer string `json:"imapServer"`
	IMAPPort   int    `json:"imapPort"`
	SMTPServer string `json:"smtpServer"`
	SMTPPort   int    `json:"smtpPort"`
	// EncryptedPassword is api.EncryptJSON(password, config.Encryption.Key).
	EncryptedPassword string `json:"encryptedPassword"`
}

// accountsStore persists connected accounts per owner via the KV seam.
type accountsStore struct {
	kv storage.KV
}

func newAccountsStore(kv storage.KV) *accountsStore { return &accountsStore{kv: kv} }

func connAccountKey(owner, email string) string { return owner + "|" + email }

var errAccountsQuotaFull = errors.New("connected account quota reached")

// errBadKeyComponent is returned when an owner or account email carries the "|"
// key delimiter (or a NUL). The composite key is "<owner>|<email>"; if either
// component itself contains "|", the delimiter is ambiguous and the list prefix
// scan could match a DIFFERENT owner's records (e.g. owner "a" enumerating owner
// "a|b"'s accounts). Rejecting the delimiter fail-closed keeps every record
// addressable by exactly one owner and makes the per-owner isolation structural
// rather than reliant on a first-"|" heuristic. Emails cannot legally contain an
// unquoted "|", so this rejects only crafted/hostile input.
var errBadKeyComponent = errors.New("owner/email contains an illegal key delimiter")

// validKeyComponent reports whether s is safe to place in a composite KV key: no
// "|" delimiter and no NUL.
func validKeyComponent(s string) bool {
	return !strings.ContainsAny(s, "|\x00")
}

// list returns owner's connected accounts, sorted by email. Only records under the
// "<owner>|" prefix are returned; the decoded owner is re-checked defensively.
func (s *accountsStore) list(owner string) ([]connectedAccount, error) {
	if s == nil || s.kv == nil {
		return nil, errors.New("accounts storage unavailable")
	}
	// An owner carrying "|" would make the prefix scan ambiguous (it could match a
	// different owner's records). Fail closed rather than risk a cross-owner read.
	if !validKeyComponent(owner) {
		return nil, errBadKeyComponent
	}
	m, err := s.kv.List(connAccountsNS, owner+"|")
	if err != nil {
		return nil, err
	}
	prefix := owner + "|"
	out := make([]connectedAccount, 0, len(m))
	for k, b := range m {
		// The stored key is "<owner>|<email>" and BOTH components are "|"-free (the
		// delimiter is rejected at save time). So a key that belongs to THIS owner
		// has exactly one "|": the prefix we scanned on. If the remainder after the
		// prefix still contains a "|", the key really belongs to a longer owner
		// (e.g. "a|b|email" is owner "a|b", not owner "a") — skip it. This closes
		// the cross-owner enumeration where a first-"|" split would misattribute a
		// neighbouring owner's record to this one.
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if strings.Contains(k[len(prefix):], "|") {
			continue
		}
		var a connectedAccount
		if json.Unmarshal(b, &a) != nil {
			continue // skip a corrupt record rather than fail the whole list
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// get returns a single account owned by owner, or ErrNotFound. Keyed by (owner,
// email) so another user's account is unaddressable → 404 no-leak.
func (s *accountsStore) get(owner, email string) (*connectedAccount, error) {
	if s == nil || s.kv == nil {
		return nil, storage.ErrNotFound
	}
	// A "|" in either component makes the composite key ambiguous → treat as a
	// miss (404 no-leak) rather than let it address a neighbouring owner's record.
	if !validKeyComponent(owner) || !validKeyComponent(email) {
		return nil, storage.ErrNotFound
	}
	b, err := s.kv.Get(connAccountsNS, connAccountKey(owner, email))
	if err != nil {
		return nil, err
	}
	var a connectedAccount
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// save upserts an account for owner, enforcing the per-user quota on CREATE (a
// record whose key does not already exist), so a re-save/edit never trips it.
func (s *accountsStore) save(owner string, a connectedAccount) error {
	if s == nil || s.kv == nil {
		return errors.New("accounts storage unavailable")
	}
	if owner == "" || a.Email == "" {
		return errors.New("accounts: owner and email required")
	}
	// Reject the key delimiter in either component so a stored record is always
	// addressable by exactly one owner (no ambiguous "<owner>|<email>" split).
	if !validKeyComponent(owner) || !validKeyComponent(a.Email) {
		return errBadKeyComponent
	}
	k := connAccountKey(owner, a.Email)
	if _, err := s.kv.Get(connAccountsNS, k); errors.Is(err, storage.ErrNotFound) {
		existing, lerr := s.list(owner)
		if lerr != nil {
			return lerr
		}
		if len(existing) >= maxConnectedAccounts {
			return errAccountsQuotaFull
		}
	}
	b, err := json.Marshal(&a)
	if err != nil {
		return err
	}
	return s.kv.Set(connAccountsNS, k, b)
}

// delete removes an account owned by owner. Deleting a missing/foreign key is a
// no-op; the handler translates presence into 200 vs 404 itself.
func (s *accountsStore) delete(owner, email string) error {
	if s == nil || s.kv == nil {
		return nil
	}
	if !validKeyComponent(owner) || !validKeyComponent(email) {
		return nil // unaddressable key → nothing of this owner's to delete
	}
	return s.kv.Delete(connAccountsNS, connAccountKey(owner, email))
}
