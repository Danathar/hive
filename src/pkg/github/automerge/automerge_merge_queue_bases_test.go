package automerge

import (
	"testing"
	"time"
)

// TestMergeQueueBasesSharedAcrossEngines pins that one MergeQueueBases passed
// through Options is what every engine reads and writes: the governor builds a
// one-shot engine per tick for the queued, trusted-author and contributor-lane
// sweeps, and the merge-eligibility classifier reads the same memory.
func TestMergeQueueBasesSharedAcrossEngines(t *testing.T) {
	now := time.Date(2026, 10, 25, 17, 25, 0, 0, time.UTC)
	shared := NewMergeQueueBases()
	a := New(nil, Options{MergeQueueBases: shared, Now: func() time.Time { return now }})
	b := New(nil, Options{MergeQueueBases: shared, Now: func() time.Time { return now }})
	private := New(nil, Options{Now: func() time.Time { return now }})

	a.noteMergeQueueBase("projectbluefin", "bluefin-bling", "main")
	if !b.mergeQueueBaseKnown("projectbluefin", "bluefin-bling", "main") {
		t.Error("an engine sharing the memory does not see the base another engine noted")
	}
	if !shared.Required("ProjectBluefin/Bluefin-Bling", "main", now) {
		t.Error("Required is not case-insensitive on the repo")
	}
	if shared.Required("projectbluefin/bluefin-bling", "testing", now) || shared.Required("projectbluefin/bluefin-bling", "", now) {
		t.Error("another base, or an unknown one, reads as merge-queue-only")
	}
	if private.mergeQueueBaseKnown("projectbluefin", "bluefin-bling", "main") {
		t.Error("an engine built without Options.MergeQueueBases sees the shared memory")
	}
	if shared.Required("projectbluefin/bluefin-bling", "main", now.Add(mergeQueueBaseRecheck)) {
		t.Error("entry did not expire after mergeQueueBaseRecheck")
	}
	var nilMem *MergeQueueBases
	nilMem.Note("o/r", "main", now)
	if nilMem.Required("o/r", "main", now) {
		t.Error("a nil memory reports a base")
	}
}
