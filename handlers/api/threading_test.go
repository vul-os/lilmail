package api

import (
	"lilmail/models"
	"testing"
	"time"
)

// makeEmail is a convenience constructor for test emails.
func makeEmail(id, msgID, inReplyTo string, refs []string, subject string, t time.Time) models.Email {
	return models.Email{
		ID:         id,
		MessageID:  msgID,
		InReplyTo:  inReplyTo,
		References: refs,
		Subject:    subject,
		Date:       t,
		From:       "user@example.com",
	}
}

var (
	t0 = time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 = time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)
	t2 = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	t3 = time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC)
)

// TestThreadMessages_SingleMessage ensures a lone message becomes one thread
// with Count==1.
func TestThreadMessages_SingleMessage(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Hello", t0),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}
	th := threads[0]
	if th.Count != 1 {
		t.Errorf("expected Count=1, got %d", th.Count)
	}
	if th.Root.ID != "1" {
		t.Errorf("expected root ID=1, got %s", th.Root.ID)
	}
}

// TestThreadMessages_TwoInThread ensures a reply is grouped with its parent.
func TestThreadMessages_TwoInThread(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Hello", t0),
		makeEmail("2", "<b@host>", "<a@host>", []string{"<a@host>"}, "Re: Hello", t1),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d: %v", len(threads), threadIDs(threads))
	}
	if threads[0].Count != 2 {
		t.Errorf("expected Count=2, got %d", threads[0].Count)
	}
}

// TestThreadMessages_TwoSeparateThreads ensures unrelated messages stay separate.
func TestThreadMessages_TwoSeparateThreads(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Hello", t0),
		makeEmail("2", "<b@host>", "", nil, "World", t1),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(threads))
	}
}

// TestThreadMessages_ChainOfThree checks a References chain A→B→C is one thread.
func TestThreadMessages_ChainOfThree(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Topic", t0),
		makeEmail("2", "<b@host>", "<a@host>", []string{"<a@host>"}, "Re: Topic", t1),
		makeEmail("3", "<c@host>", "<b@host>", []string{"<a@host>", "<b@host>"}, "Re: Topic", t2),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d: %v", len(threads), threadIDs(threads))
	}
	if threads[0].Count != 3 {
		t.Errorf("expected Count=3, got %d", threads[0].Count)
	}
}

// TestThreadMessages_SortedByLatest ensures the thread with the newest message
// comes first.
func TestThreadMessages_SortedByLatest(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "OldThread", t0),
		makeEmail("2", "<b@host>", "", nil, "NewThread", t3),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(threads))
	}
	if threads[0].Latest.ID != "2" {
		t.Errorf("expected newest thread first (ID=2), got ID=%s", threads[0].Latest.ID)
	}
}

// TestThreadMessages_MsgsWithinThreadSortedAsc ensures messages within a
// thread are in ascending date order.
func TestThreadMessages_MsgsWithinThreadSortedAsc(t *testing.T) {
	emails := []models.Email{
		makeEmail("3", "<c@host>", "<b@host>", []string{"<a@host>", "<b@host>"}, "Re: T", t2),
		makeEmail("1", "<a@host>", "", nil, "T", t0),
		makeEmail("2", "<b@host>", "<a@host>", []string{"<a@host>"}, "Re: T", t1),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}
	msgs := threads[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 msgs, got %d", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Date.Before(msgs[i-1].Date) {
			t.Errorf("messages not in ascending date order at index %d", i)
		}
	}
}

// TestThreadMessages_RootIsEarliest checks that Root holds the earliest message.
func TestThreadMessages_RootIsEarliest(t *testing.T) {
	emails := []models.Email{
		makeEmail("2", "<b@host>", "<a@host>", []string{"<a@host>"}, "Re: X", t1),
		makeEmail("1", "<a@host>", "", nil, "X", t0),
	}
	threads := ThreadMessages(emails)
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}
	if threads[0].Root.ID != "1" {
		t.Errorf("expected root ID=1 (earliest), got %s", threads[0].Root.ID)
	}
}

// TestThreadMessages_SubjectGrouping checks that messages with matching
// normalized subjects but no References are grouped together.
func TestThreadMessages_SubjectGrouping(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Quarterly Report", t0),
		makeEmail("2", "<b@host>", "", nil, "Re: Quarterly Report", t1),
	}
	threads := ThreadMessages(emails)
	// They share a subject so should end up in one thread.
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread via subject grouping, got %d: %v", len(threads), threadIDs(threads))
	}
	if threads[0].Count != 2 {
		t.Errorf("expected Count=2, got %d", threads[0].Count)
	}
}

// TestThreadMessages_DuplicateMessageID checks that duplicate Message-IDs are
// handled gracefully (not duplicated in the thread).
func TestThreadMessages_DuplicateMessageID(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "<a@host>", "", nil, "Dup", t0),
		makeEmail("2", "<a@host>", "", nil, "Dup", t1), // same Message-ID
	}
	// Should not panic and must produce at least one thread.
	threads := ThreadMessages(emails)
	if len(threads) == 0 {
		t.Fatal("expected at least one thread")
	}
}

// TestThreadMessages_NoMessageID checks that messages without a Message-ID
// still appear as single-message threads.
func TestThreadMessages_NoMessageID(t *testing.T) {
	emails := []models.Email{
		{ID: "1", Subject: "No ID", Date: t0},
		{ID: "2", Subject: "No ID 2", Date: t1},
	}
	threads := ThreadMessages(emails)
	total := 0
	for _, th := range threads {
		total += th.Count
	}
	if total < 2 {
		t.Errorf("expected at least 2 messages across threads, got %d", total)
	}
}

// TestThreadMessages_Empty ensures no panic on empty input.
func TestThreadMessages_Empty(t *testing.T) {
	threads := ThreadMessages(nil)
	if threads != nil {
		t.Errorf("expected nil for empty input, got %v", threads)
	}
}

// TestNormalizeSubject checks subject normalization.
func TestNormalizeSubject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello", "hello"},
		{"Re: Hello", "hello"},
		{"RE: Hello", "hello"},
		{"Fwd: Hello", "hello"},
		{"FWD: Hello", "hello"},
		{"Re: Re: Hello", "hello"},
		{"Fw: Hello", "hello"},
		{"re: fwd: Test", "test"},
	}
	for _, tc := range cases {
		got := normalizeSubject(tc.in)
		if got != tc.want {
			t.Errorf("normalizeSubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// threadIDs is a helper for error messages.
func threadIDs(threads []models.Thread) []string {
	var ids []string
	for _, th := range threads {
		for _, m := range th.Messages {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
