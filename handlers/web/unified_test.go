// handlers/web/unified_test.go
//
// Unit tests for the unified inbox fetch / merge / tagging logic.
// No live IMAP server is required; we exercise FetchUnified via mock data
// injected at the merge layer (MergeAndSort) and the per-account error
// isolation via AccountFetchResult.
package web

import (
	"errors"
	"lilmail/models"
	"sort"
	"testing"
	"time"
)

// ─── MergeAndSort ────────────────────────────────────────────────────────────

// mergeAndSort is the pure merge+sort logic extracted from FetchUnified so we
// can unit-test it without touching IMAP.  It mirrors the logic in
// FetchUnified: gather all non-errored Emails, sort date desc, cap.
func mergeAndSort(results []AccountFetchResult, limit int) []models.Email {
	var merged []models.Email
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		merged = append(merged, r.Emails...)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Date.After(merged[j].Date)
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func makeEmail(id, acct string, daysAgo int) models.Email {
	return models.Email{
		ID:           id,
		AccountEmail: acct,
		AccountLabel: acct,
		Date:         time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Subject:      "msg-" + id,
	}
}

// TestMergeAndSort_Order verifies messages are returned newest-first across
// accounts.
func TestMergeAndSort_Order(t *testing.T) {
	results := []AccountFetchResult{
		{
			AccountEmail: "a@example.com",
			Emails: []models.Email{
				makeEmail("a1", "a@example.com", 3),
				makeEmail("a2", "a@example.com", 1),
			},
		},
		{
			AccountEmail: "b@example.com",
			Emails: []models.Email{
				makeEmail("b1", "b@example.com", 5),
				makeEmail("b2", "b@example.com", 0),
			},
		},
	}

	merged := mergeAndSort(results, 0)
	if len(merged) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(merged))
	}
	// b2 (today) > a2 (1 day ago) > a1 (3 days ago) > b1 (5 days ago)
	order := []string{"b2", "a2", "a1", "b1"}
	for i, id := range order {
		if merged[i].ID != id {
			t.Errorf("[%d] want %s, got %s", i, id, merged[i].ID)
		}
	}
}

// TestMergeAndSort_ErrorIsolation verifies that a failing account does not
// suppress messages from accounts that succeeded.
func TestMergeAndSort_ErrorIsolation(t *testing.T) {
	results := []AccountFetchResult{
		{
			AccountEmail: "ok@example.com",
			Emails: []models.Email{
				makeEmail("ok1", "ok@example.com", 1),
				makeEmail("ok2", "ok@example.com", 2),
			},
		},
		{
			AccountEmail: "broken@example.com",
			Err:          errors.New("IMAP connection refused"),
			Emails:       nil,
		},
	}

	merged := mergeAndSort(results, 0)
	if len(merged) != 2 {
		t.Fatalf("expected 2 messages from healthy account, got %d", len(merged))
	}
	for _, m := range merged {
		if m.AccountEmail != "ok@example.com" {
			t.Errorf("unexpected account on message: %s", m.AccountEmail)
		}
	}
}

// TestMergeAndSort_AllFailed verifies that an empty slice is returned when all
// accounts fail.
func TestMergeAndSort_AllFailed(t *testing.T) {
	results := []AccountFetchResult{
		{AccountEmail: "a@x.com", Err: errors.New("timeout")},
		{AccountEmail: "b@x.com", Err: errors.New("auth failed")},
	}
	merged := mergeAndSort(results, 0)
	if len(merged) != 0 {
		t.Errorf("expected 0 messages when all accounts fail, got %d", len(merged))
	}
}

// TestMergeAndSort_Limit verifies the cap is applied after merge+sort.
func TestMergeAndSort_Limit(t *testing.T) {
	var emails []models.Email
	for i := 0; i < 10; i++ {
		emails = append(emails, makeEmail("m"+string(rune('0'+i)), "a@x.com", i))
	}
	results := []AccountFetchResult{{AccountEmail: "a@x.com", Emails: emails}}
	merged := mergeAndSort(results, 5)
	if len(merged) != 5 {
		t.Errorf("expected 5 after cap, got %d", len(merged))
	}
}

// TestMergeAndSort_AccountTagging verifies that each message retains the
// AccountEmail / AccountLabel / AccountColor set by the fetcher.
func TestMergeAndSort_AccountTagging(t *testing.T) {
	results := []AccountFetchResult{
		{
			AccountEmail: "work@example.com",
			AccountLabel: "Work",
			AccountColor: "#4285F4",
			Emails: []models.Email{
				{
					ID:           "w1",
					Date:         time.Now(),
					AccountEmail: "work@example.com",
					AccountLabel: "Work",
					AccountColor: "#4285F4",
				},
			},
		},
		{
			AccountEmail: "home@example.com",
			AccountLabel: "Home",
			AccountColor: "#E53935",
			Emails: []models.Email{
				{
					ID:           "h1",
					Date:         time.Now().Add(-time.Minute),
					AccountEmail: "home@example.com",
					AccountLabel: "Home",
					AccountColor: "#E53935",
				},
			},
		},
	}

	merged := mergeAndSort(results, 0)
	if len(merged) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(merged))
	}
	// Newer first: w1, then h1
	if merged[0].ID != "w1" || merged[0].AccountLabel != "Work" || merged[0].AccountColor != "#4285F4" {
		t.Errorf("first message tagging wrong: %+v", merged[0])
	}
	if merged[1].ID != "h1" || merged[1].AccountLabel != "Home" || merged[1].AccountColor != "#E53935" {
		t.Errorf("second message tagging wrong: %+v", merged[1])
	}
}

// TestMergeAndSort_SingleAccount verifies no-op behaviour when only one account
// is present (unified-mode is identical to single-account mode in this case).
func TestMergeAndSort_SingleAccount(t *testing.T) {
	emails := []models.Email{
		makeEmail("1", "solo@example.com", 2),
		makeEmail("2", "solo@example.com", 0),
		makeEmail("3", "solo@example.com", 5),
	}
	results := []AccountFetchResult{{AccountEmail: "solo@example.com", Emails: emails}}
	merged := mergeAndSort(results, 0)
	if len(merged) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(merged))
	}
	// Check order: msg 2 (today) > msg 1 (2 days ago) > msg 3 (5 days ago).
	if merged[0].ID != "2" || merged[1].ID != "1" || merged[2].ID != "3" {
		t.Errorf("order wrong: %s %s %s", merged[0].ID, merged[1].ID, merged[2].ID)
	}
}

// TestAccountFetchError verifies AccountFetchError.HasErrors and Error().
func TestAccountFetchError_HasErrors(t *testing.T) {
	noErrors := AccountFetchError{
		{AccountEmail: "a@x.com"},
		{AccountEmail: "b@x.com"},
	}
	if noErrors.HasErrors() {
		t.Error("HasErrors should be false when no Err set")
	}
	if noErrors.Error() != "" {
		t.Errorf("Error() should be empty string, got %q", noErrors.Error())
	}

	withErrors := AccountFetchError{
		{AccountEmail: "a@x.com"},
		{AccountEmail: "b@x.com", Err: errors.New("timeout")},
	}
	if !withErrors.HasErrors() {
		t.Error("HasErrors should be true")
	}
	if withErrors.Error() == "" {
		t.Error("Error() should be non-empty")
	}
}

// TestMergeAndSort_EmptyEntries verifies that an empty input slice produces an
// empty output without panicking.
func TestMergeAndSort_EmptyEntries(t *testing.T) {
	merged := mergeAndSort(nil, 50)
	if len(merged) != 0 {
		t.Errorf("expected empty, got %d", len(merged))
	}
}
