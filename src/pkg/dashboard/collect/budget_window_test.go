package collect

// Tracker-level ports of pkg/dashboard's budget-history tests
// (kubestellar/hive#5565 slice 2): the same scenarios driven directly through
// BudgetWindowTracker.Observe instead of Server.ObserveBudgetWindow. The
// Server-mediated versions (status parsing, the /api/budget/history endpoint)
// stay in pkg/dashboard.

import (
	"encoding/json"
	"testing"
	"time"
)

func ms(t time.Time) int64 { return t.UnixMilli() }

// The roll is detected on the observation AFTER it happened, by which time the
// live spend has already reset toward zero — the recorded row must carry the
// high-water mark, not the post-reset reading.
func TestTrackerWindowRollRecordsPeakNotPostResetReading(t *testing.T) {
	tr := &BudgetWindowTracker{}
	start := time.Now().Add(-7 * 24 * time.Hour)
	end := time.Now()

	if rolled := tr.Observe(ms(start), ms(end), 1_000_000, 200_000, false); rolled != nil {
		t.Fatalf("first observation must not close a window, got %+v", rolled)
	}
	tr.Observe(ms(start), ms(end), 1_000_000, 900_000, false) // high water
	tr.Observe(ms(start), ms(end), 1_000_000, 950_000, true)  // exhausted latches

	// Window rolls: new bounds, spend back near zero.
	newStart := end
	newEnd := end.Add(7 * 24 * time.Hour)
	rolled := tr.Observe(ms(newStart), ms(newEnd), 1_000_000, 10_000, false)
	if rolled == nil {
		t.Fatal("window change must emit the closed row")
	}
	if rolled.Used != 950_000 {
		t.Errorf("Used = %d, want the high-water 950000, never the post-reset reading", rolled.Used)
	}
	if !rolled.Exhausted {
		t.Error("Exhausted must latch across later non-exhausted observations")
	}
	if rolled.WindowStart != ms(start) || rolled.WindowEnd != ms(end) {
		t.Errorf("closed row bounds = [%d,%d], want [%d,%d]", rolled.WindowStart, rolled.WindowEnd, ms(start), ms(end))
	}
}

// "Did the budget stop the hive while I was away?" — after several rolls the
// history answers per closed window, newest first.
func TestTrackerHistoryNewestFirst(t *testing.T) {
	tr := &BudgetWindowTracker{}
	base := time.Now().Add(-4 * 7 * 24 * time.Hour)
	week := 7 * 24 * time.Hour
	for i := 0; i < 3; i++ {
		start := base.Add(time.Duration(i) * week)
		end := start.Add(week)
		tr.Observe(ms(start), ms(end), 100, int64(10*(i+1)), i == 1)
	}
	// Close the third window by reporting no open window.
	tr.Observe(0, 0, 0, 0, false)

	hist := tr.Snapshot()
	if len(hist) != 3 {
		t.Fatalf("history = %d rows, want 3", len(hist))
	}
	if hist[0].Used != 30 || hist[2].Used != 10 {
		t.Errorf("history not newest-first: %+v", hist)
	}
	if !hist[1].Exhausted {
		t.Errorf("middle window must record exhaustion: %+v", hist[1])
	}
}

// The limit in force FOR THE WINDOW is recorded, so raising the limit later
// never rewrites what a past window ran under; PctUsed derives from it.
func TestTrackerLimitIsRecordedPerWindow(t *testing.T) {
	tr := &BudgetWindowTracker{}
	start := time.Now().Add(-7 * 24 * time.Hour)
	end := time.Now()
	tr.Observe(ms(start), ms(end), 500, 500, true)

	newEnd := end.Add(7 * 24 * time.Hour)
	rolled := tr.Observe(ms(end), ms(newEnd), 2_000, 0, false)
	if rolled == nil {
		t.Fatal("roll must emit the closed row")
	}
	if rolled.Limit != 500 {
		t.Errorf("Limit = %d, want the 500 in force for the closed window", rolled.Limit)
	}
	if rolled.PctUsed != 100 {
		t.Errorf("PctUsed = %v, want 100", rolled.PctUsed)
	}
}

// A zero windowEnd (no limit configured) closes any tracked window without
// starting a new one, and observing nothing records nothing.
func TestTrackerNoBudgetConfiguredRecordsNothing(t *testing.T) {
	tr := &BudgetWindowTracker{}
	if rolled := tr.Observe(0, 0, 0, 0, false); rolled != nil {
		t.Fatalf("idle tracker must not emit rows, got %+v", rolled)
	}
	if got := tr.Snapshot(); len(got) != 0 {
		t.Fatalf("history = %+v, want empty", got)
	}
}

// Retention: the ring keeps only the newest BudgetWindowMaxEntries rows.
func TestTrackerRetentionKeepsNewest(t *testing.T) {
	tr := &BudgetWindowTracker{}
	week := 7 * 24 * time.Hour
	start := time.Now().Add(-time.Duration(BudgetWindowMaxEntries+10) * week)
	total := BudgetWindowMaxEntries + 5
	for i := 0; i < total; i++ {
		ws := start.Add(time.Duration(i) * week)
		we := ws.Add(week)
		tr.Observe(ms(ws), ms(we), 100, int64(i), false)
	}
	tr.Observe(0, 0, 0, 0, false) // close the last one

	hist := tr.Snapshot()
	if len(hist) != BudgetWindowMaxEntries {
		t.Fatalf("history = %d entries, want the cap %d", len(hist), BudgetWindowMaxEntries)
	}
	if hist[0].Used != int64(total-1) {
		t.Errorf("newest row Used = %d, want %d (retention must drop the OLDEST)", hist[0].Used, total-1)
	}
}

// Seed restores persisted history: a Snapshot → JSON → Seed round trip
// preserves both content and order, and over-long input truncates to newest.
func TestTrackerSeedRoundTripsThroughJSON(t *testing.T) {
	tr := &BudgetWindowTracker{}
	week := 7 * 24 * time.Hour
	base := time.Now().Add(-3 * week)
	for i := 0; i < 2; i++ {
		ws := base.Add(time.Duration(i) * week)
		tr.Observe(ms(ws), ms(ws.Add(week)), 100, int64(10*(i+1)), false)
	}
	tr.Observe(0, 0, 0, 0, false)
	saved := tr.Snapshot()

	blob, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var loaded []BudgetWindowEntry
	if err := json.Unmarshal(blob, &loaded); err != nil {
		t.Fatal(err)
	}

	restarted := &BudgetWindowTracker{}
	restarted.Seed(loaded)
	got := restarted.Snapshot()
	if len(got) != len(saved) {
		t.Fatalf("restored %d rows, want %d", len(got), len(saved))
	}
	for i := range got {
		if got[i] != saved[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], saved[i])
		}
	}

	// Over-long input keeps the NEWEST cap-many entries rather than rejecting.
	long := make([]BudgetWindowEntry, BudgetWindowMaxEntries+4)
	for i := range long {
		long[i] = BudgetWindowEntry{Used: int64(len(long) - i)} // newest-first
	}
	over := &BudgetWindowTracker{}
	over.Seed(long)
	trimmed := over.Snapshot()
	if len(trimmed) != BudgetWindowMaxEntries {
		t.Fatalf("seeded %d rows, want cap %d", len(trimmed), BudgetWindowMaxEntries)
	}
	if trimmed[0].Used != long[0].Used {
		t.Errorf("truncation must keep the newest entries: got %+v, want %+v", trimmed[0], long[0])
	}
}
