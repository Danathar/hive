package collect

// Moved from pkg/dashboard with the ActivityCollector (kubestellar/hive#5565
// slice 2). The collector tests drive the aggregation through a stub
// AuditReader; the audit-file READING behavior (OutputActionsSince over
// rotated/compressed backups) stays tested next to AuditLog in pkg/dashboard.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// stubAuditReader is a canned AuditReader so Start/collect run without a real
// audit log on disk.
type stubAuditReader struct {
	entries []AuditEntry
	calls   atomic.Int32
}

func (f *stubAuditReader) OutputActionsSince(time.Time, map[string]bool, string) []AuditEntry {
	f.calls.Add(1)
	return f.entries
}

// collect groups by repo + action with counts and newest timestamps, across
// multiple repos — the multi-repo case that broke the hand-scraped counts.
func TestActivityCollector_CountsPerRepo(t *testing.T) {
	now := time.Now().UTC()
	newest := now.Add(-10 * time.Minute)
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "agent_issue_created", Detail: "repo=z/ui, number=1, agent=quality"},
		{Timestamp: rfc3339(newest), Action: "agent_issue_created", Detail: "repo=z/ui, number=2", Agent: "docs"},
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=z/api-server, number=9", Agent: "quality"},
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), Action: "pr_merged", Detail: "repo=z/api-server, number=8", Agent: "governor"},
		{Timestamp: rfc3339(now.Add(-4 * time.Hour)), Action: "agent_pr_reviewed", Detail: "repo=z/ui, number=5, state=approved", Agent: "reviewer"},
		{Timestamp: rfc3339(now.Add(-5 * time.Hour)), Action: "agent_issue_claimed", Detail: "repo=z/ui-framework, number=3", Agent: "quality"},
		{Timestamp: rfc3339(now.Add(-6 * time.Hour)), Action: "pr_attribution_reconciled", Detail: "repo=z/ui, number=6", Agent: "governor"},
	}}
	ac := NewActivityCollector(stub, "", nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()
	snap, ok := ac.Snapshot()
	if !ok {
		t.Fatal("snapshot not ready after collect")
	}
	if len(snap.Repos) != 3 {
		t.Fatalf("want 3 repos, got %d", len(snap.Repos))
	}
	byRepo := map[string]RepoActivity{}
	for _, r := range snap.Repos {
		byRepo[r.Repo] = r
	}
	if byRepo["z/ui"].Issues.Count != 2 {
		t.Errorf("z/ui issues = %d, want 2", byRepo["z/ui"].Issues.Count)
	}
	if byRepo["z/ui"].Issues.NewestAt != rfc3339(newest) {
		t.Errorf("z/ui newest issue = %q, want %q", byRepo["z/ui"].Issues.NewestAt, rfc3339(newest))
	}
	if byRepo["z/ui"].Reviews.Count != 1 {
		t.Errorf("z/ui reviews = %d, want 1", byRepo["z/ui"].Reviews.Count)
	}
	if byRepo["z/ui"].Reconciled.Count != 1 {
		t.Errorf("z/ui reconciled = %d, want 1", byRepo["z/ui"].Reconciled.Count)
	}
	uiAgents := map[string]AgentRepoActivity{}
	for _, a := range byRepo["z/ui"].Agents {
		uiAgents[a.Agent] = a
	}
	if uiAgents["quality"].Issues.Count != 1 || uiAgents["docs"].Issues.Count != 1 || uiAgents["reviewer"].Reviews.Count != 1 {
		t.Errorf("z/ui per-agent counts = %+v, want quality/docs issues and reviewer review", uiAgents)
	}
	if byRepo["z/api-server"].PRs.Count != 1 || byRepo["z/api-server"].Merges.Count != 1 {
		t.Errorf("z/api-server prs/merges = %d/%d, want 1/1", byRepo["z/api-server"].PRs.Count, byRepo["z/api-server"].Merges.Count)
	}
	if byRepo["z/ui-framework"].Claims.Count != 1 {
		t.Errorf("z/ui-framework claims = %d, want 1", byRepo["z/ui-framework"].Claims.Count)
	}
	if snap.WindowHours != activityHealthWindowHours {
		t.Errorf("window hours = %d, want %d", snap.WindowHours, activityHealthWindowHours)
	}
}

// Entries with no repo= stay out of repo rows and are reported explicitly as
// unattributed rather than smeared across known repos.
func TestActivityCollector_SkipsNoRepo(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "advisory_commented", Detail: "flow=advisory-digest"}, // no repo=
	}}
	ac := NewActivityCollector(stub, "", nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()
	snap, _ := ac.Snapshot()
	if len(snap.Repos) != 0 {
		t.Errorf("entry without repo= must be skipped, got %+v", snap.Repos)
	}
	if snap.Unattributed.Count != 1 {
		t.Errorf("unattributed count = %d, want 1", snap.Unattributed.Count)
	}
}

// EnablePersistence round-trips a snapshot across a restart.
func TestActivityCollector_PersistRestore(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=o/r, number=1"},
	}}
	sidecar := filepath.Join(t.TempDir(), "activity.json")

	ac := NewActivityCollector(stub, "", nil)
	ac.nowFn = func() time.Time { return now }
	ac.EnablePersistence(sidecar)
	ac.collect()
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}

	// Fresh collector with an EMPTY audit reader but the sidecar present →
	// restores the prior snapshot.
	ac2 := NewActivityCollector(&stubAuditReader{}, "", nil)
	ac2.EnablePersistence(sidecar)
	snap, ok := ac2.Snapshot()
	if !ok || len(snap.Repos) != 1 || snap.Repos[0].PRs.Count != 1 {
		t.Errorf("restore failed: ok=%v snap=%+v", ok, snap)
	}
}

// Start is inert with a nil audit reader (no panic, returns).
func TestActivityCollector_NilAuditInert(t *testing.T) {
	ac := NewActivityCollector(nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ac.Start(ctx) // must return immediately
	if _, ok := ac.Snapshot(); ok {
		t.Error("nil-audit collector must not be ready")
	}
}

// TestActivitySnapshotWindowsAreDistinct pins the two windows apart. window_hours
// is the hub's FRESHNESS window; count_window_hours is the lookback Count was
// accumulated over. #4860: the snapshot advertised only the 12h freshness value
// while counting over 14d, so any consumer deriving a rate overstated activity
// by 28x. Re-coupling them (or dropping count_window_hours) must fail here
// rather than silently reappear as a wrong number on a cost row later.
func TestActivitySnapshotWindowsAreDistinct(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubAuditReader{entries: []AuditEntry{{
		Timestamp: rfc3339(now.Add(-1 * time.Hour)),
		Action:    "agent_pr_created",
		Detail:    "repo=o/r, number=1",
		Agent:     "quality",
	}}}
	ac := NewActivityCollector(stub, "", nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()

	snap, ready := ac.Snapshot()
	if !ready {
		t.Fatal("snapshot not ready")
	}
	if snap.WindowHours != activityHealthWindowHours {
		t.Errorf("WindowHours = %d, want %d (the freshness window)",
			snap.WindowHours, activityHealthWindowHours)
	}
	wantCount := int(activityWindow / time.Hour)
	if snap.CountWindowHours != wantCount {
		t.Errorf("CountWindowHours = %d, want %d (the accumulation window)",
			snap.CountWindowHours, wantCount)
	}
	// The bug was reporting one value for both. If a future edit makes them
	// equal, the 28x rate error is back and this is the signal.
	if snap.CountWindowHours == snap.WindowHours {
		t.Fatalf("count and freshness windows must stay distinct; both = %d", snap.WindowHours)
	}
	// The count window must cover the freshness window, or a "fresh" event
	// could fall outside the counted range.
	if snap.CountWindowHours < snap.WindowHours {
		t.Fatalf("CountWindowHours (%d) must be >= WindowHours (%d)",
			snap.CountWindowHours, snap.WindowHours)
	}
}

func TestActivityCollector_CollectedAt(t *testing.T) {
	var nilAC *ActivityCollector
	if !nilAC.CollectedAt().IsZero() {
		t.Error("nil collector CollectedAt should be zero")
	}

	ac := NewActivityCollector(&stubAuditReader{}, "", nil)
	if !ac.CollectedAt().IsZero() {
		t.Error("fresh collector CollectedAt should be zero")
	}
	ac.collect()
	if ac.CollectedAt().IsZero() {
		t.Error("CollectedAt should be set after a collect")
	}
}

// AuditPath echoes the configured path (and "" on a nil collector) so the
// dashboard's repo-cost fallback can read the identical file set.
func TestActivityCollector_AuditPath(t *testing.T) {
	var nilAC *ActivityCollector
	if got := nilAC.AuditPath(); got != "" {
		t.Errorf("nil collector AuditPath = %q, want empty", got)
	}
	ac := NewActivityCollector(&stubAuditReader{}, "/data/custom.jsonl", nil)
	if got := ac.AuditPath(); got != "/data/custom.jsonl" {
		t.Errorf("AuditPath = %q, want the configured path", got)
	}
}

// Start must be a no-op on a nil collector and on one with no audit reader.
func TestActivityCollector_StartInert(t *testing.T) {
	done := make(chan struct{})
	go func() {
		var nilAC *ActivityCollector
		nilAC.Start(context.Background())
		NewActivityCollector(nil, "", nil).Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return immediately for inert collectors")
	}
}

// Start collects once up front, again on each tick, and exits on ctx cancel.
func TestActivityCollector_StartCollectsAndStops(t *testing.T) {
	oldInterval := activityCollectInterval
	activityCollectInterval = 5 * time.Millisecond
	defer func() { activityCollectInterval = oldInterval }()

	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(time.Now()), Action: "agent_pr_created", Detail: "repo=o/r, agent=quality"},
	}}
	ac := NewActivityCollector(stub, "ignored", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ac.Start(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for stub.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after ctx cancel")
	}
	if stub.calls.Load() < 2 {
		t.Errorf("collect calls = %d, want >=2 (upfront + tick)", stub.calls.Load())
	}
	snap, ready := ac.Snapshot()
	if !ready {
		t.Fatal("snapshot should be ready after collect")
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Repo != "o/r" {
		t.Errorf("snapshot repos = %+v, want one entry for o/r", snap.Repos)
	}
}

// persistLocked failure paths: an unwritable temp path and a rename target that
// is a directory must both be swallowed (logged), never panic or persist junk.
func TestActivityCollector_PersistLockedFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// No persist path configured → early return.
	ac := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac.persistLocked()

	// Temp-file write fails: parent directory does not exist.
	ac.persistPath = filepath.Join(t.TempDir(), "missing-subdir", "activity.json")
	ac.persistLocked()

	// Rename fails: destination is an existing directory.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "activity.json")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	ac.persistPath = blocked
	ac.persistLocked()
	if _, err := os.Stat(filepath.Join(dir, "activity.json.tmp")); err != nil {
		t.Fatalf("temp sidecar should exist after failed rename: %v", err)
	}
}

// EnablePersistence restore paths: corrupt JSON and a zero collected_at are
// both "start fresh"; a valid sidecar restores snapshot + timestamp.
func TestActivityCollector_EnablePersistenceRestore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	// Corrupt sidecar → logged, ignored, not ready.
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ac := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac.EnablePersistence(corrupt)
	if _, ready := ac.Snapshot(); ready {
		t.Error("corrupt sidecar must not mark the collector ready")
	}

	// Zero collected_at → treated as empty, not ready.
	zero := filepath.Join(dir, "zero.json")
	if err := os.WriteFile(zero, []byte(`{"snapshot":{},"collected_at":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ac2 := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac2.EnablePersistence(zero)
	if _, ready := ac2.Snapshot(); ready {
		t.Error("zero collected_at must not mark the collector ready")
	}

	// Round-trip: collect+persist in one collector, restore in a second.
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(time.Now()), Action: "pr_merged", Detail: "repo=o/persisted, agent=quality"},
	}}
	sidecar := filepath.Join(dir, "activity.json")
	writer := NewActivityCollector(stub, "ignored", logger)
	writer.EnablePersistence(sidecar)
	writer.collect()

	restored := NewActivityCollector(&stubAuditReader{}, "", logger)
	restored.EnablePersistence(sidecar)
	snap, ready := restored.Snapshot()
	if !ready {
		t.Fatal("restored collector should be ready")
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Repo != "o/persisted" {
		t.Errorf("restored repos = %+v, want o/persisted", snap.Repos)
	}
	if restored.CollectedAt().IsZero() {
		t.Error("restored CollectedAt should be non-zero")
	}
}
