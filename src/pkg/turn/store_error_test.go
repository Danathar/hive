package turn

// Ported from v4's #6189 store/envelope/journal error-branch tests. The
// FileStore and envelope-version cases target v4's FileStore{Path}/Load()
// API, which v5's turn package replaced (FileStore{Dir}, Load(ctx, id),
// no envelope version), so only the journal and executor cases carry over.

import (
	"testing"
	"time"
)

func TestJournalAmbiguousReturnsOnlyIntendedEntries(t *testing.T) {
	j := Journal{Entries: []JournalEntry{
		{IdempotencyKey: "k-done", Status: OpSucceeded},
		{IdempotencyKey: "k-open", Status: OpIntended},
		{IdempotencyKey: "k-failed", Status: OpFailed},
		{IdempotencyKey: "k-open-2", Status: OpIntended},
	}}
	got := j.Ambiguous()
	if len(got) != 2 {
		t.Fatalf("Ambiguous() returned %d entries, want 2: %+v", len(got), got)
	}
	if got[0].IdempotencyKey != "k-open" || got[1].IdempotencyKey != "k-open-2" {
		t.Fatalf("Ambiguous() = %+v, want the two intended entries in order", got)
	}

	empty := Journal{}
	if entries := empty.Ambiguous(); entries != nil {
		t.Fatalf("Ambiguous() on empty journal = %+v, want nil", entries)
	}
}

func TestExecutorNowFallsBackToWallClock(t *testing.T) {
	x := &JournaledExecutor{}
	before := time.Now().UTC().Add(-time.Second)
	got := x.now()
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("now() = %v, want within [%v, %v]", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("now() location = %v, want UTC", got.Location())
	}

	fixed := time.Date(2026, 9, 7, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
	x.Now = func() time.Time { return fixed }
	// v5's now() returns the injected clock unchanged (no UTC normalisation);
	// only the wall-clock fallback is pinned to UTC.
	if got := x.now(); !got.Equal(fixed) {
		t.Fatalf("now() with injected clock = %v, want %v", got, fixed)
	}
}
