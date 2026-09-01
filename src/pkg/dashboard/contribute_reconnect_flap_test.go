package dashboard

import (
	"testing"
	"time"
)

// The tests in this file cover the two halves of the contributor-flap work:
// kubestellar/hive#5151 (a ~1s reconnect must not be booked as a full departure
// plus arrival) and kubestellar/hive#5090 (the heartbeat loop must not report a
// ping failure on a socket somebody else already tore down).

// flapRows drives the exact three-row sequence one flap writes:
// "released: connection lost" -> "left" -> "joined".
func flapRows(hub *ContributeWSHub, user, task string) {
	hub.addActivity(user, "released: connection lost", "contributor", "claude", "m", "", task)
	hub.addActivity(user, "left", "contributor", "claude", "m", "", "")
	hub.addActivity(user, "joined", "contributor", "claude", "m", "", "")
}

func actions(entries []ActivityEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Action
	}
	return out
}

// TestReconnectFlap_CollapsesToSingleJoined is the core #5151 assertion: a flap
// leaves ONE row, not three, and the surviving row is the "joined" that proves the
// contributor is present. The prior "picked up" — the row an operator actually
// wants and that this churn was evicting — must survive untouched.
func TestReconnectFlap_CollapsesToSingleJoined(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("alice", "picked up", "contributor", "claude", "m", "", "myorg/repo#7")
	flapRows(hub, "alice", "myorg/repo#7")

	got := actions(hub.RecentActivity())
	want := []string{"picked up", "joined"}
	if len(got) != len(want) {
		t.Fatalf("flap should collapse to %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activity = %v, want %v", got, want)
		}
	}

	if n := hub.AbsorbedReconnects(); n != 1 {
		t.Fatalf("absorbed reconnect counter = %d, want 1 — the flap must stay countable", n)
	}
}

// TestReconnectFlap_ManyFlapsDoNotEvictTheFeed pins the concrete harm #5151
// reports: at three rows per flap against maxActivityEntries (50), a flapping
// contributor churns the whole retained feed in under 20 minutes, evicting every
// real row. Twenty flaps must not evict the surrounding history.
func TestReconnectFlap_ManyFlapsDoNotEvictTheFeed(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("alice", "picked up", "contributor", "claude", "m", "", "myorg/repo#7")
	for i := 0; i < 20; i++ {
		flapRows(hub, "alice", "myorg/repo#7")
	}

	got := hub.RecentActivity()
	if len(got) > 3 {
		t.Fatalf("20 flaps should not fill the feed, got %d rows: %v", len(got), actions(got))
	}
	if got[0].Action != "picked up" {
		t.Fatalf("the real 'picked up' row was evicted by flap churn: %v", actions(got))
	}
	if n := hub.AbsorbedReconnects(); n != 20 {
		t.Fatalf("absorbed reconnect counter = %d, want 20", n)
	}
}

// TestGenuineDeparture_KeepsEveryRow is the falling-through case #5151 requires:
// "a grace period that swallows a genuine departure would be worse than the
// churn". With no reconnect, all rows stand exactly as they do today.
func TestGenuineDeparture_KeepsEveryRow(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("alice", "released: connection lost", "contributor", "claude", "m", "", "myorg/repo#7")
	hub.addActivity("alice", "left", "contributor", "claude", "m", "", "")

	got := actions(hub.RecentActivity())
	if len(got) != 2 || got[0] != "released: connection lost" || got[1] != "left" {
		t.Fatalf("a departure with no reconnect must keep every row, got %v", got)
	}
	if n := hub.AbsorbedReconnects(); n != 0 {
		t.Fatalf("nothing was absorbed, counter = %d, want 0", n)
	}
}

// TestReconnectFlap_DoesNotCollapseAcrossUsers guards the narrowness of the walk
// back: bob's departure must not be retracted by alice arriving.
func TestReconnectFlap_DoesNotCollapseAcrossUsers(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("bob", "left", "contributor", "claude", "m", "", "")
	hub.addActivity("alice", "joined", "contributor", "claude", "m", "", "")

	got := actions(hub.RecentActivity())
	if len(got) != 2 || got[0] != "left" {
		t.Fatalf("another user's 'left' must not be absorbed, got %v", got)
	}
	if hub.RecentActivity()[0].Username != "bob" {
		t.Fatalf("bob's departure row was retracted by alice's arrival")
	}
}

// TestReconnectFlap_StaleLeftIsNotAbsorbed checks the window bound: a "left" from
// outside reconnectFlapWindow is a real departure, and a later arrival is a real
// arrival. Both rows stand.
func TestReconnectFlap_StaleLeftIsNotAbsorbed(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("alice", "left", "contributor", "claude", "m", "", "")

	// Backdate the departure past the window.
	hub.activityMu.Lock()
	hub.activity[0].Timestamp = time.Now().Add(-2 * reconnectFlapWindow).UTC().Format(time.RFC3339)
	hub.activityMu.Unlock()

	hub.addActivity("alice", "joined", "contributor", "claude", "m", "", "")

	got := actions(hub.RecentActivity())
	if len(got) != 2 || got[0] != "left" || got[1] != "joined" {
		t.Fatalf("a departure older than the flap window must not be absorbed, got %v", got)
	}
}

// TestReconnectFlap_BareReleasedRowSurvives ensures a "released: connection lost"
// is only ever retracted as part of a left+joined round trip. On its own it
// describes WORK, not presence, and a subsequent join must not erase it.
func TestReconnectFlap_BareReleasedRowSurvives(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.addActivity("alice", "released: connection lost", "contributor", "claude", "m", "", "myorg/repo#7")
	hub.addActivity("alice", "joined", "contributor", "claude", "m", "", "")

	got := actions(hub.RecentActivity())
	if len(got) != 2 || got[0] != "released: connection lost" {
		t.Fatalf("a released row with no 'left' must survive, got %v", got)
	}
}

// TestReconnectFlap_PreservesDuplicatePRGuarantee is the #2356 invariant #5151
// explicitly asks to be pinned by a test.
//
// The duplicate-PR guarantee lives in the release cooldown, NOT in the activity
// feed. The failure mode this guards against is a "fix" that defers or suppresses
// bookReleaseCooldown behind a grace timer, which would reopen the window in which
// selectTask can hand the same issue to a second session while the original relay
// is still working it. The feed collapse must be provably orthogonal: after a full
// flap has been absorbed down to one row, the issue must STILL be in failure
// cooldown.
func TestReconnectFlap_PreservesDuplicatePRGuarantee(t *testing.T) {
	hub, _ := covK2Hub(t)

	// A disconnect books the #2356 hedge on the in-flight issue...
	hub.bookReleaseCooldown("myorg/repo", 7)
	// ...and writes the three feed rows, which the reconnect then absorbs.
	flapRows(hub, "alice", "myorg/repo#7")

	if len(hub.RecentActivity()) != 1 {
		t.Fatalf("precondition: the flap should have been absorbed, got %v",
			actions(hub.RecentActivity()))
	}
	if !hub.isTaskInFailureCooldown("myorg/repo", 7) {
		t.Fatal("#2356 REGRESSION: absorbing the feed rows must not withdraw the " +
			"release cooldown — the duplicate-PR window would be reopened")
	}
}

// TestReleaseCooldown_WithdrawnOnlyByLeaseBoundResume documents the one sanctioned
// way the #2356 hedge is withdrawn (#5322): the original owner re-entering
// activeIssues via a lease-bound resume, which is the STRONGER guard the cooldown
// was standing in for. Absorbing feed rows is not that, and must not imitate it.
func TestReleaseCooldown_WithdrawnOnlyByLeaseBoundResume(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.bookReleaseCooldown("myorg/repo", 7)
	if !hub.isTaskInFailureCooldown("myorg/repo", 7) {
		t.Fatal("precondition: cooldown should be booked")
	}

	// The resume path's withdrawal.
	hub.clearReleaseCooldown("myorg/repo", 7)
	if hub.isTaskInFailureCooldown("myorg/repo", 7) {
		t.Fatal("a lease-bound resume should withdraw the speculative hedge")
	}

	// And it stays narrow: a cooldown carrying a real consecutive-failure count is
	// a genuine failure record and must NOT be launderable by a resume.
	hub.recordTaskFailure("myorg/repo", 8, false)
	hub.completedMu.Lock()
	hub.consecutiveFailures["myorg/repo#8"] = 2
	hub.completedMu.Unlock()

	hub.clearReleaseCooldown("myorg/repo", 8)
	if !hub.isTaskInFailureCooldown("myorg/repo", 8) {
		t.Fatal("a real failure record must not be withdrawn by a resume")
	}
}

// TestHeartbeatLoop_SilentOnDeregisteredConnection is the #5090 assertion.
//
// The "heartbeat ping failed, closing" line fired ~29-30s after EVERY new
// connection because the heartbeat tick is a fixed offset from registration: the
// read loop's disconnect defer had already deleted the connection and closed the
// socket, and this loop — which has no done channel — slept out its interval and
// then wrote to a corpse. The resulting log line read as a cause and was a
// lagging indicator by up to a full heartbeat interval, which is what sent #5090's
// diagnosis toward a per-direction proxy idle timer.
//
// connectionRegistered is the guard. Asserting it directly is what matters: a
// connection that is not in h.connections must be reported as gone, so the loop
// returns before it can write and mislabel.
func TestHeartbeatLoop_SilentOnDeregisteredConnection(t *testing.T) {
	hub, _ := covK2Hub(t)

	c := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "alice"}}

	if hub.connectionRegistered(c) {
		t.Fatal("an unregistered connection must not be reported as registered")
	}

	hub.mu.Lock()
	hub.connections["conn-1"] = c
	hub.mu.Unlock()
	if !hub.connectionRegistered(c) {
		t.Fatal("a registered connection must be reported as registered")
	}

	// What the disconnect defer does on the read goroutine.
	hub.mu.Lock()
	delete(hub.connections, "conn-1")
	hub.mu.Unlock()
	if hub.connectionRegistered(c) {
		t.Fatal("after deregistration the heartbeat loop must stop rather than " +
			"write to a torn-down socket and log a misleading ping failure")
	}

	// Identity is by pointer, not by username: a reconnect registering a NEW
	// connection for the same contributor must not keep the OLD loop alive.
	replacement := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "alice"}}
	hub.mu.Lock()
	hub.connections["conn-2"] = replacement
	hub.mu.Unlock()
	if hub.connectionRegistered(c) {
		t.Fatal("the superseded connection must not be kept alive by its replacement")
	}
	if !hub.connectionRegistered(replacement) {
		t.Fatal("the replacement connection should be registered")
	}
}

// TestWriteDeadline_IsBoundedAndUnderHeartbeatInterval pins the relationship the
// #5090 write-deadline fix depends on. A write must not still be parked when the
// next heartbeat tick arrives (which would stack ticker goroutines on writeMu),
// and it must be comfortably longer than the control-frame deadline so an
// ordinarily slow client is never mistaken for a wedged one.
func TestWriteDeadline_IsBoundedAndUnderHeartbeatInterval(t *testing.T) {
	if wsWriteDeadline <= 0 {
		t.Fatal("an unbounded write deadline is the defect: WriteJSON on a " +
			"half-open socket blocks indefinitely while holding writeMu")
	}
	if wsWriteDeadline >= wsHeartbeatInterval {
		t.Fatalf("wsWriteDeadline (%v) must be shorter than wsHeartbeatInterval (%v) "+
			"so a wedged write cannot outlive the tick that started it",
			wsWriteDeadline, wsHeartbeatInterval)
	}
	if wsWriteDeadline <= wsProtocolPingDeadline {
		t.Fatalf("wsWriteDeadline (%v) should exceed wsProtocolPingDeadline (%v)",
			wsWriteDeadline, wsProtocolPingDeadline)
	}
}
