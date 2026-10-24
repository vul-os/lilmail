package web

import (
	"errors"
	"lilmail/handlers/api"
	"lilmail/models"
	"testing"
)

// flagClient records SetMessageFlag calls; every other MailClient method is nil.
type flagClient struct {
	api.MailClient
	err   error
	calls []string
}

func (f *flagClient) SetMessageFlag(folder, uid, flag string, add bool) error {
	f.calls = append(f.calls, folder+"/"+uid+" "+flag)
	if !add {
		return errors.New("unexpected removal")
	}
	return f.err
}

// Opening a message must mark it read: it is fetched with BODY.PEEK on a
// read-only SELECT, so the server never sets \Seen itself.
func TestMarkReadOnOpen(t *testing.T) {
	e := models.Email{Flags: []string{`\Flagged`}}
	fc := &flagClient{}
	markReadOnOpen(fc, "INBOX", "42", &e)
	if len(fc.calls) != 1 || fc.calls[0] != `INBOX/42 \Seen` {
		t.Fatalf("calls = %v", fc.calls)
	}
	if !hasSeenFlag(e.Flags) || len(e.Flags) != 2 {
		t.Fatalf("flags = %v", e.Flags)
	}
}

func TestMarkReadOnOpen_AlreadySeen(t *testing.T) {
	e := models.Email{Flags: []string{`\seen`}}
	fc := &flagClient{}
	markReadOnOpen(fc, "INBOX", "42", &e)
	if len(fc.calls) != 0 || len(e.Flags) != 1 {
		t.Fatalf("a read message must not be stored again: calls=%v flags=%v", fc.calls, e.Flags)
	}
}

// A failing STORE must leave the flags alone and must not panic: the viewer
// still has to render.
func TestMarkReadOnOpen_StoreFailureIsHarmless(t *testing.T) {
	e := models.Email{}
	fc := &flagClient{err: errors.New("read-only mailbox")}
	markReadOnOpen(fc, "INBOX", "42", &e)
	if hasSeenFlag(e.Flags) {
		t.Fatalf("flags must not claim \\Seen after a failed STORE: %v", e.Flags)
	}
}
