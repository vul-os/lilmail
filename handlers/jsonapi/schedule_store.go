// handlers/jsonapi/schedule_store.go — durable persistence for scheduled sends.
//
// A scheduled send is a compose payload the caller asked to deliver at a future
// instant (POST /v1/messages with sendAt). It is persisted here so it survives a
// restart, then a poll-based drain (see schedule.go) picks it up at its due time
// and sends it through the SAME BuildMIMEMessage → SMTP path as an immediate send
// — so the wave-49 header-injection guard and wave-44 cid: handling both still
// apply at actual-send time.
//
// STORAGE: one record per scheduled send in the storage.KV seam (bbolt by
// default, Postgres when configured — identical to snooze/rules durability),
// namespace "scheduled_sends". The key is "<account>|<id>" so:
//   - every record is OWNED by exactly one account (the key prefix), giving
//     per-account isolation for free (List(prefix="<account>|")), and
//   - a cross-account read/cancel simply cannot address another account's record
//     (a DELETE keyed on the WRONG account's prefix misses → 404 no-leak).
//
// SECRET-AT-REST: the outbound SMTP credential captured at schedule time is
// stored ENCRYPTED with the same config.Encryption.Key the session store uses
// (api.EncryptJSON), never in plaintext. In CP-brokered mode the per-request
// token is short-lived and must NOT be persisted for a far-future send; the
// broker path therefore refuses to schedule beyond the token horizon (see
// schedule.go) — honest degrade rather than a stale-credential silent drop.
package jsonapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"lilmail/handlers/api"
	"lilmail/storage"
)

// scheduleNS is the KV namespace holding scheduled-send records.
const scheduleNS = "scheduled_sends"

// maxPendingPerAccount bounds how many scheduled sends a single account may have
// queued at once, so a caller cannot fill durable storage with an unbounded queue
// (a cheap DoS). Chosen generously for legitimate "send-later" use.
const maxPendingPerAccount = 100

// scheduledSend is the persisted record. Everything needed to rebuild + send the
// message at fire time is captured here; NOTHING is read from the (long-gone)
// request context when the drain fires.
type scheduledSend struct {
	ID      string `json:"id"`      // stable, opaque; used as the dedup key
	Account string `json:"account"` // owner identity (== From); the authz boundary
	SendAt  int64  `json:"sendAt"`  // unix seconds; fire at-or-after this instant
	Created int64  `json:"created"` // unix seconds; for display/ordering

	// Compose payload — rebuilt into MIME at fire time (guard re-runs there).
	From        string                   `json:"from"`
	To          string                   `json:"to"`
	Cc          string                   `json:"cc"`
	Bcc         string                   `json:"bcc"`
	Subject     string                   `json:"subject"`
	Text        string                   `json:"text"`
	HTML        string                   `json:"html"`
	InReplyTo   string                   `json:"inReplyTo"`
	Attachments []api.OutgoingAttachment `json:"attachments"`

	// Transport — how to reach the account's SMTP server at fire time. The Secret
	// is stored ENCRYPTED (see EncSecret); plaintext Secret is only ever populated
	// transiently in memory right before a send.
	SMTPHost     string `json:"smtpHost"`
	SMTPPort     int    `json:"smtpPort"`
	SMTPUseOAuth bool   `json:"smtpUseOauth"`
	OAuthMech    string `json:"oauthMech"`
	UseSTARTTLS  bool   `json:"useStartTls"`
	InsecureSkip bool   `json:"insecureSkip"`
	EncSecret    string `json:"encSecret"` // api.EncryptJSON(secret, key)
	Secret       string `json:"-"`         // never serialized; in-memory only

	// Attempts counts how many times the drain has tried (and failed) to SEND this
	// record. It bounds the at-least-once retry loop: a permanently-failing send
	// (e.g. credentials revoked after a password change, or a hard 5xx reject) must
	// eventually be abandoned rather than re-dialed SMTP forever — which would pin
	// the encrypted credential in storage and burn a quota slot indefinitely. See
	// maxSendAttempts in schedule.go.
	Attempts int `json:"attempts"`
}

// scheduleStore persists scheduledSend records via the KV seam. All access is
// keyed by (account, id) so an account can only ever see/mutate its own records.
type scheduleStore struct {
	kv     storage.KV
	encKey string // config.Encryption.Key; "" disables at-rest secret storage
}

func newScheduleStore(kv storage.KV, encKey string) *scheduleStore {
	return &scheduleStore{kv: kv, encKey: encKey}
}

// key composes the storage key for a record. The account prefix is the isolation
// boundary; "|" cannot appear in the id (ids are hex) so the split is unambiguous.
func scheduleKey(account, id string) string {
	return account + "|" + id
}

// newScheduleID returns a 128-bit random hex id (opaque, unguessable, stable).
func newScheduleID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var errScheduleQuotaFull = errors.New("scheduled send quota reached for this account")

// Put persists a record, encrypting the transport secret at rest. It enforces the
// per-account quota on CREATE (a record whose key does not already exist).
func (s *scheduleStore) Put(rec *scheduledSend) error {
	if s == nil || s.kv == nil {
		return errors.New("scheduled send storage unavailable")
	}
	if rec.Account == "" || rec.ID == "" {
		return errors.New("scheduled send: account and id required")
	}
	k := scheduleKey(rec.Account, rec.ID)

	// Quota is checked only when creating a new record (not on edit/PATCH), so a
	// legitimate edit of an existing send never trips it.
	if _, err := s.kv.Get(scheduleNS, k); errors.Is(err, storage.ErrNotFound) {
		pending, err := s.List(rec.Account)
		if err != nil {
			return err
		}
		if len(pending) >= maxPendingPerAccount {
			return errScheduleQuotaFull
		}
	}

	// Encrypt the transport secret at rest; never persist the plaintext.
	out := *rec
	out.Secret = ""
	if rec.Secret != "" && s.encKey != "" {
		enc, err := api.EncryptJSON(rec.Secret, s.encKey)
		if err != nil {
			return fmt.Errorf("encrypt scheduled-send secret: %w", err)
		}
		out.EncSecret = enc
	}
	b, err := json.Marshal(&out)
	if err != nil {
		return err
	}
	return s.kv.Set(scheduleNS, k, b)
}

// Get returns a single record OWNED by account, or ErrNotFound. The account is
// part of the key, so requesting another account's id simply misses — there is no
// path by which one account reads another's record.
func (s *scheduleStore) Get(account, id string) (*scheduledSend, error) {
	if s == nil || s.kv == nil {
		return nil, storage.ErrNotFound
	}
	b, err := s.kv.Get(scheduleNS, scheduleKey(account, id))
	if err != nil {
		return nil, err
	}
	var rec scheduledSend
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	// Defense in depth: never hand back a record whose stored owner disagrees with
	// the requesting account (should be impossible given the key, but cheap).
	if rec.Account != account {
		return nil, storage.ErrNotFound
	}
	return &rec, nil
}

// Delete removes a record owned by account. Deleting a missing/foreign key is a
// no-op (the handler translates "was it there?" into 200 vs 404 itself).
func (s *scheduleStore) Delete(account, id string) error {
	if s == nil || s.kv == nil {
		return nil
	}
	return s.kv.Delete(scheduleNS, scheduleKey(account, id))
}

// List returns an account's pending records, soonest-due first. Records are read
// via the "<account>|" key prefix, so no other account's records are returned.
func (s *scheduleStore) List(account string) ([]*scheduledSend, error) {
	if s == nil || s.kv == nil {
		return nil, nil
	}
	m, err := s.kv.List(scheduleNS, account+"|")
	if err != nil {
		return nil, err
	}
	out := make([]*scheduledSend, 0, len(m))
	for _, b := range m {
		var rec scheduledSend
		if err := json.Unmarshal(b, &rec); err != nil {
			continue // skip a corrupt record rather than fail the whole list
		}
		// Prefix match can be fooled only if an account name itself contains "|";
		// re-verify the decoded owner to be certain.
		if rec.Account != account {
			continue
		}
		out = append(out, &rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SendAt < out[j].SendAt })
	return out, nil
}

// listAllDue returns every record across all accounts whose SendAt is at-or-before
// `now`. Used by the drain to find overdue sends (including restart catch-up). The
// caller decrypts + sends each; this method never touches the network.
func (s *scheduleStore) listAllDue(now time.Time) ([]*scheduledSend, error) {
	if s == nil || s.kv == nil {
		return nil, nil
	}
	m, err := s.kv.List(scheduleNS, "")
	if err != nil {
		return nil, err
	}
	cutoff := now.Unix()
	var out []*scheduledSend
	for _, b := range m {
		var rec scheduledSend
		if err := json.Unmarshal(b, &rec); err != nil {
			continue
		}
		if rec.SendAt <= cutoff {
			out = append(out, &rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SendAt < out[j].SendAt })
	return out, nil
}

// decryptSecret populates rec.Secret from rec.EncSecret in place. It is called by
// the drain just before building the SMTP client, so the plaintext secret lives
// only transiently in memory. A record with no EncSecret (or no key) simply keeps
// an empty Secret and the send fails cleanly at SMTP auth.
func (s *scheduleStore) decryptSecret(rec *scheduledSend) error {
	if rec.EncSecret == "" || s.encKey == "" {
		return nil
	}
	var plain string
	if err := api.DecryptJSON(rec.EncSecret, &plain, s.encKey); err != nil {
		return err
	}
	rec.Secret = plain
	return nil
}

// scheduleRedactPublic returns the client-safe view of a record (no secrets, no
// full body dump beyond what the client needs to render its pending list).
func scheduleRedactPublic(rec *scheduledSend) map[string]any {
	return map[string]any{
		"id":          rec.ID,
		"to":          rec.To,
		"cc":          rec.Cc,
		"subject":     rec.Subject,
		"sendAt":      time.Unix(rec.SendAt, 0).UTC().Format(time.RFC3339),
		"created":     time.Unix(rec.Created, 0).UTC().Format(time.RFC3339),
		"hasBody":     strings.TrimSpace(rec.Text) != "" || strings.TrimSpace(rec.HTML) != "",
		"attachments": len(rec.Attachments),
	}
}
