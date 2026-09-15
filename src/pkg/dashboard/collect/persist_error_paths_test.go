package collect

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tokens"
)

// These tests pin the FAILURE paths of the two collector PVC persist helpers —
// FleetStatsCollector.persistLocked and RepoCostCollector.persistLocked (the
// session store counterpart lives in pkg/dashboard). Their happy paths were already covered by the
// persist-and-reload tests; what was not covered is the best-effort contract on
// a sick PVC: a failed WriteFile or Rename must warn (never panic, never error
// out of the collect/login path) and must leave the in-memory state serving.
//
// The failures are provoked without chmod (which is a no-op for root):
//   - WriteFile fails because <path>.tmp already exists as a DIRECTORY.
//   - Rename fails because <path> itself is a non-empty directory.

// blockTmp makes WriteFile(path+".tmp") fail by planting a directory there.
func blockTmp(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path+".tmp", 0o755); err != nil {
		t.Fatalf("plant tmp-blocking dir: %v", err)
	}
}

// blockRename makes Rename(path+".tmp", path) fail by planting a non-empty
// directory at path.
func blockRename(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatalf("plant rename-blocking dir: %v", err)
	}
}

// warnLogger returns a logger writing to buf so the tests can assert the
// failure was reported, not swallowed.
func warnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestFleetStatsPersist_WriteAndRenameFailures(t *testing.T) {
	cases := []struct {
		name     string
		block    func(t *testing.T, path string)
		wantWarn string
	}{
		{"tmp write fails", blockTmp, "failed to write fleet stats store"},
		{"rename fails", blockRename, "failed to replace fleet stats store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet-stats.json")
			tc.block(t, path)

			var buf bytes.Buffer
			c := newFleetTestClient(t, map[string]int{"is:merged merged:>=": 7})
			fc := NewFleetStatsCollector(c, "bot", "org", warnLogger(&buf))
			fc.EnablePersistence(path)

			// collect() drives persistLocked; it must complete despite the
			// persist failure and keep the counts in memory.
			fc.collect(context.Background())

			got, ready := fc.Snapshot()
			if !ready {
				t.Fatal("collector must stay ready when persist fails — persistence is best-effort")
			}
			if got.PRsMerged != 7 {
				t.Fatalf("in-memory counts lost on persist failure: %+v", got)
			}
			if !strings.Contains(buf.String(), tc.wantWarn) {
				t.Fatalf("expected warn %q, log was: %s", tc.wantWarn, buf.String())
			}
		})
	}
}

func TestRepoCostPersist_WriteAndRenameFailures(t *testing.T) {
	cases := []struct {
		name     string
		block    func(t *testing.T, path string)
		wantWarn string
	}{
		{"tmp write fails", blockTmp, "failed to write repo-cost store"},
		{"rename fails", blockRename, "failed to replace repo-cost store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repo-cost.json")
			tc.block(t, path)

			var buf bytes.Buffer
			rc := NewRepoCostCollector(&fakeFixedAudit{},
				&fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", warnLogger(&buf))
			rc.EnablePersistence(path)

			// collect() drives persistLocked and must survive its failure.
			rc.collect()

			if _, ready := rc.Snapshot(); !ready {
				t.Fatal("collector must stay ready when persist fails — persistence is best-effort")
			}
			if !strings.Contains(buf.String(), tc.wantWarn) {
				t.Fatalf("expected warn %q, log was: %s", tc.wantWarn, buf.String())
			}
		})
	}
}
