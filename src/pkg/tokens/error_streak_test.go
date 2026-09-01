package tokens

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Helpers building a synthetic bob home: trustedFolders.json maps a folder to
// its basename (the agent), and chat recordings live under
// tmp/<sha256(folder)>/chats/.

func writeBobHome(t *testing.T, agents map[string]string) string {
	t.Helper()
	home := t.TempDir()
	folders := map[string]string{}
	for folder := range agents {
		folders[folder] = "trusted"
	}
	b, err := json.Marshal(folders)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "trustedFolders.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeBobChat(t *testing.T, home, folder, sessionID string, sess map[string]any) {
	t.Helper()
	sum := sha256.Sum256([]byte(folder))
	hash := hexLower(sum[:])
	dir := filepath.Join(home, "tmp", hash, "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess["projectHash"] = hash
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ts(base time.Time, minutes int) string {
	return base.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
}

// The #5338 shape: an agent whose earlier turns carried real usage, then every
// later call died — some recorded as zero-token replies, some as kicks that
// never got a reply at all. The streak counts the consecutive tail failures
// and stops at the last success.
func TestBobAgentErrorStreaks_CrashLoopCounted(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := now.Add(-3 * time.Hour)
	folder := "/data/agents/quality"
	home := writeBobHome(t, map[string]string{folder: "quality"})

	writeBobChat(t, home, folder, "s1", map[string]any{
		"sessionId":   "s1",
		"startTime":   ts(base, 0),
		"lastUpdated": ts(base, 120),
		"messages": []map[string]any{
			// A successful turn: usage recorded.
			{"type": "user", "timestamp": ts(base, 0), "content": "kick 1"},
			{"type": "bob-shell", "timestamp": ts(base, 1), "model": "premium",
				"tokens": map[string]int64{"input": 100, "output": 20}},
			// Failure recorded as an explicit zero-usage reply.
			{"type": "user", "timestamp": ts(base, 30), "content": "kick 2"},
			{"type": "bob-shell", "timestamp": ts(base, 31), "model": "premium",
				"tokens": map[string]int64{"input": 0, "output": 0}},
			// Failures recorded as kicks with no reply at all (the call
			// crashed before responding).
			{"type": "user", "timestamp": ts(base, 60), "content": "kick 3"},
			{"type": "user", "timestamp": ts(base, 90), "content": "kick 4"},
		},
	})

	got := BobAgentErrorStreaks(home, now)
	if got["quality"] != 3 {
		t.Fatalf("streak = %v, want quality:3", got)
	}
}

// A trailing unanswered turn younger than the grace window is an in-flight
// kick, not a failure — but an explicit zero-usage reply counts regardless.
func TestBobAgentErrorStreaks_InFlightTurnNotCounted(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := now.Add(-time.Hour)
	folder := "/data/agents/scanner"
	home := writeBobHome(t, map[string]string{folder: "scanner"})

	writeBobChat(t, home, folder, "s1", map[string]any{
		"sessionId":   "s1",
		"startTime":   ts(base, 0),
		"lastUpdated": now.Format(time.RFC3339Nano),
		"messages": []map[string]any{
			{"type": "user", "timestamp": ts(base, 0), "content": "kick"},
			{"type": "bob-shell", "timestamp": ts(base, 1),
				"tokens": map[string]int64{"input": 50, "output": 10}},
			// Fresh kick, no reply yet: must not be flagged.
			{"type": "user", "timestamp": now.Add(-2 * time.Minute).Format(time.RFC3339Nano), "content": "kick"},
		},
	})

	if got := BobAgentErrorStreaks(home, now); got["scanner"] != 0 {
		t.Fatalf("in-flight turn counted as failure: %v", got)
	}
}

// A streak whose newest failure is older than a day is history (the agent is
// idle or was fixed), and an agent whose recordings never carry an explicit
// usage structure (estimation-era format) must be skipped entirely — in that
// format a SUCCESSFUL call records no usage either, so every turn would look
// failed.
func TestBobAgentErrorStreaks_StaleAndIncapableSkipped(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	stale := "/data/agents/stale"
	legacy := "/data/agents/legacy"
	home := writeBobHome(t, map[string]string{stale: "stale", legacy: "legacy"})

	writeBobChat(t, home, stale, "s1", map[string]any{
		"sessionId":   "s1",
		"startTime":   ts(old, 0),
		"lastUpdated": ts(old, 30),
		"messages": []map[string]any{
			{"type": "user", "timestamp": ts(old, 0), "content": "kick"},
			{"type": "bob-shell", "timestamp": ts(old, 1),
				"tokens": map[string]int64{"input": 10, "output": 5}},
			{"type": "user", "timestamp": ts(old, 20), "content": "kick"},
			{"type": "bob-shell", "timestamp": ts(old, 21),
				"tokens": map[string]int64{"input": 0, "output": 0}},
		},
	})
	// Legacy format: turns with content but no tokens/usage structures at all.
	writeBobChat(t, home, legacy, "s2", map[string]any{
		"sessionId":   "s2",
		"startTime":   ts(now.Add(-time.Hour), 0),
		"lastUpdated": ts(now.Add(-time.Hour), 30),
		"messages": []map[string]any{
			{"type": "user", "timestamp": ts(now.Add(-time.Hour), 0), "content": "kick"},
			{"type": "bob-shell", "timestamp": ts(now.Add(-time.Hour), 1), "content": "reply"},
		},
	})

	got := BobAgentErrorStreaks(home, now)
	if len(got) != 0 {
		t.Fatalf("stale/legacy streaks reported: %v", got)
	}
}

// The result is always a measurement (non-nil), and a healthy agent reports
// no entry at all rather than a zero.
func TestBobAgentErrorStreaks_HealthyIsAbsent(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := now.Add(-time.Hour)
	folder := "/data/agents/healthy"
	home := writeBobHome(t, map[string]string{folder: "healthy"})

	writeBobChat(t, home, folder, "s1", map[string]any{
		"sessionId":   "s1",
		"startTime":   ts(base, 0),
		"lastUpdated": ts(base, 10),
		"messages": []map[string]any{
			{"type": "user", "timestamp": ts(base, 0), "content": "kick"},
			{"type": "bob-shell", "timestamp": ts(base, 1),
				"tokens": map[string]int64{"input": 100, "output": 40}},
		},
	})

	got := BobAgentErrorStreaks(home, now)
	if got == nil {
		t.Fatal("result must be a non-nil measurement")
	}
	if _, ok := got["healthy"]; ok {
		t.Fatalf("healthy agent flagged: %v", got)
	}
}
