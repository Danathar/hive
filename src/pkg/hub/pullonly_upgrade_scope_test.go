package hub

import (
	"testing"
	"time"
)

// #4995 — the undeliverable-upgrade de-duplication memory used to be a
// package-level global shared by every HubServer in the process.
//
// Two defects came out of that, and these tests pin both:
//
//  1. Cross-instance clobbering. Keying was on hive ID alone, so two servers
//     managing same-named hives (the normal case for fixtures) overwrote each
//     other, and one server's memory could suppress a refusal the other should
//     have reported. The timeline entry is the whole operator-visible point of
//     pullonly_upgrade.go, so that is a silenced signal, not a tidiness issue.
//
//  2. Unbounded growth. The only removal path was "the hive was successfully
//     armed". A hive deleted while uncollectible is never armed — and an
//     unassigned placeholder that never heartbeats is exactly the population
//     this file is about — so entries outlived their hives forever.
//
// The strongest evidence for (1) was in the test file itself: 12 tests opened
// by calling forgetUncollectibleUpgrade(...) to scrub state a freshly built
// server should never have inherited. Those calls are gone as of this change,
// and that deletion is itself part of the regression coverage — if the memory
// ever becomes shared again, the existing tests start failing in combination.

// countStaleTimelineEntries reports how many "upgrade not armed" entries a
// server recorded for a hive.
func countStaleTimelineEntries(s *HubServer, hiveID string) int {
	n := 0
	for _, e := range s.timeline.recent(hiveID, 100) {
		if e.Kind == TimelineUpgradeStale {
			n++
		}
	}
	return n
}

// uncollectibleHive registers a hive that can never collect an upgrade: auto
// upgrade on, behind the branch SHA, and no heartbeat ever. That is the exact
// shape pullonly_upgrade.go exists for.
func uncollectibleHive(s *HubServer, id string) {
	saveSaaSHive(&SaaSHive{
		ID: id, Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{ID: id, GitBranch: "v4", GitHash: "old1234"}}
}

// TestUndeliverableMemoryIsNotSharedBetweenServers is the headline regression.
//
// Both servers manage a hive with the SAME id — which is what fixtures and a
// two-hub process both do. Under the package global, the first server's entry
// suppressed the second server's timeline write entirely, so an operator
// looking at the second hub saw silence where a refusal belonged.
func TestUndeliverableMemoryIsNotSharedBetweenServers(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "shared-id"

	first := pullOnlyTestServer(t)
	uncollectibleHive(first, hiveID)
	first.triggerAutoUpgrades()
	if got := countStaleTimelineEntries(first, hiveID); got != 1 {
		t.Fatalf("first server recorded %d refusal entries, want 1", got)
	}

	// A SECOND, independent server, same hive id. It has recorded nothing yet,
	// so it must report the refusal itself.
	second := pullOnlyTestServer(t)
	uncollectibleHive(second, hiveID)
	second.triggerAutoUpgrades()

	if got := countStaleTimelineEntries(second, hiveID); got != 1 {
		t.Errorf("second server recorded %d refusal entries, want 1 — "+
			"the first server's de-duplication memory suppressed a refusal it does not own (#4995)", got)
	}
	// And the first server must not have gained an entry from the second's work.
	if got := countStaleTimelineEntries(first, hiveID); got != 1 {
		t.Errorf("first server now has %d refusal entries, want 1 — the two servers are sharing state", got)
	}
}

// TestUndeliverableMemoryStartsEmptyOnAFreshServer states the property the 12
// deleted forgetUncollectibleUpgrade(...) prologue calls were compensating for.
// A server built now must inherit nothing from a server built earlier, without
// the test having to scrub anything.
func TestUndeliverableMemoryStartsEmptyOnAFreshServer(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "fresh-id"

	older := pullOnlyTestServer(t)
	uncollectibleHive(older, hiveID)
	older.triggerAutoUpgrades()

	fresh := pullOnlyTestServer(t)
	fresh.undeliverableUpgradeMu.Lock()
	n := len(fresh.undeliverableUpgradeNoted)
	fresh.undeliverableUpgradeMu.Unlock()
	if n != 0 {
		t.Errorf("a freshly built server already remembers %d undeliverable hive(s); "+
			"it must start empty so tests need no scrubbing prologue (#4995)", n)
	}
}

// TestUndeliverableMemoryIsDroppedWhenTheHiveIsGone covers the leak.
//
// A hive that is deleted while uncollectible is never armed, so arming — the
// only removal path before this change — never fires for it. Without the sweep
// prune, its entry is retained for the process lifetime and the set of dead
// placeholders grows monotonically, contradicting the "bounded" claim in the
// map's own doc comment.
func TestUndeliverableMemoryIsDroppedWhenTheHiveIsGone(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	uncollectibleHive(s, "doomed")
	s.triggerAutoUpgrades()

	s.undeliverableUpgradeMu.Lock()
	_, noted := s.undeliverableUpgradeNoted["doomed"]
	s.undeliverableUpgradeMu.Unlock()
	if !noted {
		t.Fatal("precondition failed: the uncollectible hive was never noted")
	}

	// The hive goes away, and a LIVE hive remains so the sweep sees a non-empty
	// set (an empty one is deliberately not authoritative — see
	// pruneUncollectibleUpgrades).
	removeHiveRecord("doomed", s.logger)
	saveSaaSHive(&SaaSHive{
		ID: "survivor", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "survivor", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	s.triggerAutoUpgrades()

	s.undeliverableUpgradeMu.Lock()
	_, stillNoted := s.undeliverableUpgradeNoted["doomed"]
	s.undeliverableUpgradeMu.Unlock()
	if stillNoted {
		t.Error("memory for a hive that no longer exists survived the sweep — " +
			"deleted-while-uncollectible hives are never armed, so this entry leaks forever (#4995)")
	}
}

// TestPruneKeepsEntriesForLiveHives is the control for the test above. A prune
// that simply cleared the map would pass it while destroying the whole point of
// the memory, re-emitting every suppressed refusal on the next 2-minute tick.
func TestPruneKeepsEntriesForLiveHives(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	uncollectibleHive(s, "still-here")
	s.triggerAutoUpgrades()
	if got := countStaleTimelineEntries(s, "still-here"); got != 1 {
		t.Fatalf("precondition: recorded %d entries, want 1", got)
	}

	// A second sweep with the hive still present must neither prune its entry
	// nor write a duplicate.
	s.triggerAutoUpgrades()

	s.undeliverableUpgradeMu.Lock()
	_, kept := s.undeliverableUpgradeNoted["still-here"]
	s.undeliverableUpgradeMu.Unlock()
	if !kept {
		t.Error("the prune dropped a LIVE hive's entry")
	}
	if got := countStaleTimelineEntries(s, "still-here"); got != 1 {
		t.Errorf("recorded %d refusal entries after two sweeps, want 1 — "+
			"de-duplication broke, which is the ~720-entries-a-day flood this map prevents", got)
	}
}

// TestPruneIgnoresAnEmptyHiveSet pins the read-failure guard. listSaaSHives()
// returns nil both when there are no hives and when its ReadDir fails, and the
// caller cannot tell those apart. Pruning on empty would let one transient error
// wipe the memory and re-emit every suppressed refusal.
func TestPruneIgnoresAnEmptyHiveSet(t *testing.T) {
	s := pullOnlyTestServer(t)
	s.undeliverableUpgradeNoted = map[string]string{"keep-me": "abc1234"}

	s.pruneUncollectibleUpgrades(nil)

	if _, ok := s.undeliverableUpgradeNoted["keep-me"]; !ok {
		t.Error("an empty hive list pruned the memory; a transient ReadDir failure " +
			"is indistinguishable from 'no hives' and must not wipe state (#4995)")
	}
}

// TestForgetIsScopedToItsOwnServer pins the method conversion. As a bare
// function taking only a hive ID, forgetUncollectibleUpgrade had no server to
// scope to and cleared the memory for every hub in the process.
func TestForgetIsScopedToItsOwnServer(t *testing.T) {
	a := pullOnlyTestServer(t)
	b := pullOnlyTestServer(t)
	a.undeliverableUpgradeNoted = map[string]string{"h": "sha-a"}
	b.undeliverableUpgradeNoted = map[string]string{"h": "sha-b"}

	a.forgetUncollectibleUpgrade("h")

	if _, ok := a.undeliverableUpgradeNoted["h"]; ok {
		t.Error("forget did not clear its own server's entry")
	}
	if _, ok := b.undeliverableUpgradeNoted["h"]; !ok {
		t.Error("forget on one server cleared another server's entry (#4995)")
	}
}

// TestNoteOnBareServerLiteralDoesNotPanic. Bare &HubServer{} literals are the
// norm in this package's tests and run no constructor, so the map is nil on
// first write. A nil map read is fine in Go; a write panics.
func TestNoteOnBareServerLiteralDoesNotPanic(t *testing.T) {
	s := &HubServer{timeline: newTimelineStore()}
	s.noteUncollectibleUpgrade("bare", "abc1234", "never heartbeated")

	if got := countStaleTimelineEntries(s, "bare"); got != 1 {
		t.Errorf("recorded %d entries on a bare server literal, want 1", got)
	}
	// Second call with the same target must still de-duplicate.
	s.noteUncollectibleUpgrade("bare", "abc1234", "never heartbeated")
	if got := countStaleTimelineEntries(s, "bare"); got != 1 {
		t.Errorf("recorded %d entries after a repeat, want 1", got)
	}
}
