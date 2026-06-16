// handlers/web/unified.go
//
// Multiplexed cross-account inbox fetch for the unified-inbox feature.
//
// FetchUnified opens one IMAP connection per account concurrently (goroutines),
// tags each message with the source account's label and colour, and merges the
// results into a single list sorted by date descending.
//
// Design decisions:
//   - One goroutine per account; all run concurrently.
//   - Per-account timeout via context.WithTimeout (10 s default).
//   - One account failing MUST NOT block or break the others: errors are
//     collected into AccountFetchError and returned alongside any messages that
//     did succeed.
//   - The primary session account is included; its label/colour may be empty
//     (single-account mode) — callers can handle that case.
//   - No global lock on the IMAP fetch; each goroutine owns its own connection.

package web

import (
	"context"
	"fmt"
	"lilmail/handlers/api"
	"lilmail/models"
	"log"
	"sort"
	"sync"
	"time"
)

const unifiedFetchTimeout = 10 * time.Second

// AccountFetchResult holds the messages and/or error from one account.
type AccountFetchResult struct {
	// AccountEmail is the IMAP login address for this account.
	AccountEmail string
	// AccountLabel is the display label (may equal AccountEmail when not set).
	AccountLabel string
	// AccountColor is the CSS colour string for the badge (may be empty).
	AccountColor string
	// Emails are the fetched messages, already tagged with account metadata.
	Emails []models.Email
	// Err is non-nil when the fetch for this account failed.
	Err error
}

// AccountFetchError aggregates per-account errors returned alongside a
// successful (possibly partial) unified result.
type AccountFetchError []AccountFetchResult

func (e AccountFetchError) Error() string {
	var failed []string
	for _, r := range e {
		if r.Err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.AccountEmail, r.Err))
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return fmt.Sprintf("unified fetch: %d account(s) failed: %v", len(failed), failed)
}

// HasErrors reports whether any account fetch failed.
func (e AccountFetchError) HasErrors() bool {
	for _, r := range e {
		if r.Err != nil {
			return true
		}
	}
	return false
}

// unifiedFetcher carries the dependencies needed by FetchUnified.
type unifiedFetcher struct {
	auth   *AuthHandler
	folder string
	limit  uint32
}

// fetchForEntry fetches messages for one additional (non-session) account.
// It tags each email with the account's label and colour.
func (f *unifiedFetcher) fetchForEntry(entry AccountEntry) AccountFetchResult {
	result := AccountFetchResult{
		AccountEmail: entry.Email,
		AccountLabel: entry.Label,
		AccountColor: entry.Color,
	}
	if result.AccountLabel == "" {
		result.AccountLabel = entry.Email
	}

	ctx, cancel := context.WithTimeout(context.Background(), unifiedFetchTimeout)
	defer cancel()

	done := make(chan struct{})
	var emails []models.Email
	var err error

	go func() {
		defer close(done)
		client, clientErr := f.auth.CreateIMAPClientForAccount(entry)
		if clientErr != nil {
			err = fmt.Errorf("connect: %w", clientErr)
			return
		}
		defer client.Close()

		emails, err = client.FetchMessages(f.folder, f.limit)
	}()

	select {
	case <-ctx.Done():
		result.Err = fmt.Errorf("timeout after %s", unifiedFetchTimeout)
		return result
	case <-done:
	}

	if err != nil {
		result.Err = err
		return result
	}

	// Tag each message with the source account.
	for i := range emails {
		emails[i].AccountEmail = entry.Email
		emails[i].AccountLabel = result.AccountLabel
		emails[i].AccountColor = entry.Color
	}
	result.Emails = emails
	return result
}

// FetchUnified fetches messages from the primary account (its client is already
// open and passed directly as primaryClient to avoid a double-connect) and from
// each additional account in entries, concurrently.
//
// primaryEmail / primaryLabel / primaryColor identify the session account in the
// unified list.  When primaryLabel is empty the badge is suppressed in the
// template (single-account mode).
//
// Returns:
//   - merged []models.Email sorted date descending
//   - []AccountFetchResult — one entry per account; Err != nil for failures
func FetchUnified(
	primaryClient api.MailClient,
	primaryEmail, primaryLabel, primaryColor string,
	entries []AccountEntry,
	auth *AuthHandler,
	folder string,
	limit uint32,
) ([]models.Email, []AccountFetchResult) {

	type primaryResult struct {
		emails []models.Email
		err    error
	}

	// Fetch primary account — already connected.
	primCh := make(chan primaryResult, 1)
	go func() {
		emails, err := primaryClient.FetchMessages(folder, limit)
		primCh <- primaryResult{emails, err}
	}()

	// Fetch additional accounts concurrently.
	fetcher := &unifiedFetcher{auth: auth, folder: folder, limit: limit}
	results := make([]AccountFetchResult, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(idx int, e AccountEntry) {
			defer wg.Done()
			results[idx] = fetcher.fetchForEntry(e)
		}(i, entry)
	}

	// Wait for primaries.
	prim := <-primCh
	wg.Wait()

	// Build primary result.
	primaryFetchResult := AccountFetchResult{
		AccountEmail: primaryEmail,
		AccountLabel: primaryLabel,
		AccountColor: primaryColor,
		Emails:       prim.emails,
		Err:          prim.err,
	}
	if primaryFetchResult.AccountLabel == "" {
		primaryFetchResult.AccountLabel = primaryEmail
	}

	// Tag primary emails.
	if prim.err == nil {
		for i := range primaryFetchResult.Emails {
			primaryFetchResult.Emails[i].AccountEmail = primaryEmail
			primaryFetchResult.Emails[i].AccountLabel = primaryFetchResult.AccountLabel
			primaryFetchResult.Emails[i].AccountColor = primaryColor
		}
	} else {
		log.Printf("unified: primary account %s fetch error: %v", primaryEmail, prim.err)
	}

	// Merge all results.
	allResults := append([]AccountFetchResult{primaryFetchResult}, results...)

	var merged []models.Email
	for _, r := range allResults {
		if r.Err != nil {
			continue
		}
		merged = append(merged, r.Emails...)
	}

	// Sort by date descending (most-recent first).
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Date.After(merged[j].Date)
	})

	// Cap at limit × account count to avoid unbounded growth; cap at 200.
	maxMerged := int(limit) * (len(allResults))
	if maxMerged > 200 {
		maxMerged = 200
	}
	if len(merged) > maxMerged {
		merged = merged[:maxMerged]
	}

	return merged, allResults
}
