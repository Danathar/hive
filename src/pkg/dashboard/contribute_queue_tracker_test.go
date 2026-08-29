package dashboard

import (
	"log/slog"
	"testing"
)

// contribute_queue_tracker_test.go pins the fix for kubestellar/hive#5091.
//
// A tracker/umbrella issue is coordination-only: its children carry the work and
// are queued independently, so selectTask refuses to hand the parent to anyone
// (contribute_ws.go, "skipping tracker/umbrella issue" — the gate #4188 added
// after an umbrella burned a contributor's full task budget on work only final
// integration can close).
//
// ReadyQueue is the READ-ONLY PROJECTION of the set selectTask offers from, and
// its doc comment promises the same exclusions. It read every other gate — hold,
// cooldown, no-work verdict, failure cooldown, in-flight, convergence admission,
// the title/author/label filters, skip-assigned — and not this one. The result
// was a row that looked exactly like offerable work and could never be assigned
// to anybody. Measured on a live hub: kubestellar/hive#4907 ("[Tracker] ✨
// feature: hive tui") permanently occupied a queue slot.

// seedActionableWithTracker returns a status payload holding n ordinary issues
// (1..n) plus one tracker issue numbered n+1, so a test can assert the tracker
// is dropped WITHOUT the ordinary work being dropped with it.
func seedActionableWithTracker(repo string, n int) *StatusPayload {
	payload := seedActionable(repo, n)
	payload.Repos[0].ActionableIssues = append(payload.Repos[0].ActionableIssues, map[string]any{
		"number":     float64(n + 1),
		"title":      "[Tracker] ✨ feature: umbrella",
		"labels":     []any{},
		"is_tracker": true,
	})
	return payload
}

// TestReadyQueueExcludesTrackerIssues is the core assertion: the umbrella never
// appears in the queue, and every ordinary issue still does. The second half
// matters as much as the first — a gate that also swallowed real work would be a
// worse bug than the one it fixes.
func TestReadyQueueExcludesTrackerIssues(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionableWithTracker("acme/repo", 3) // issues 1..3 + tracker #4
	s.statusMu.Unlock()

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)

	if len(q) != 3 {
		t.Fatalf("expected the 3 ordinary issues and not the tracker, got %d: %+v", len(q), q)
	}
	for _, item := range q {
		if item.Number == 4 {
			t.Errorf("tracker issue #4 is in the ready-work queue but selectTask will never offer it: %+v", item)
		}
	}
	// All three ordinary issues survive, in scan order.
	for i, want := range []int{1, 2, 3} {
		if q[i].Number != want {
			t.Errorf("ordinary issue at position %d: got #%d, want #%d", i, q[i].Number, want)
		}
	}
}

// TestReadyQueueTrackerGateDoesNotDependOnLabels guards the gate's input. The
// enumerator decides trackerhood three different ways — a "[Tracker]" title
// prefix, a "meta-tracker" label, or three-plus task-list issue references in
// the body (github.IsTrackerIssue) — and hands the queue the ALREADY-COMPUTED
// is_tracker flag. A future refactor that re-derived the answer from labels
// alone would silently re-admit the #4907 case, which carries no labels at all.
func TestReadyQueueTrackerGateDoesNotDependOnLabels(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "acme/repo", Full: "acme/repo",
		ActionableIssues: []any{
			// No labels, ordinary-looking title — exactly #4907's shape once the
			// enumerator has classified it from the body's task list.
			map[string]any{
				"number":     float64(7),
				"title":      "✨ feature: a perfectly ordinary title",
				"labels":     []any{},
				"is_tracker": true,
			},
		},
	}}}
	s.statusMu.Unlock()

	if q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit); len(q) != 0 {
		t.Errorf("an unlabelled tracker must still be excluded, got %+v", q)
	}
}

// TestReadyQueueHeldTrackerStillSurfacesAsHeld pins the gate's PLACEMENT, not
// just its existence. The tracker check sits after the operator HOLD block, so a
// held tracker keeps rendering as held — the operator's manual decision stays the
// stronger, visible signal, exactly as the hold block's own comment requires for
// the held-and-cooled case. Moving the tracker gate above the hold would make an
// operator's parked row vanish from their view with no explanation.
func TestReadyQueueHeldTrackerStillSurfacesAsHeld(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionableWithTracker("acme/repo", 1) // issue #1 + tracker #2
	s.statusMu.Unlock()

	s.deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#2"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("expected the ordinary issue plus the held tracker, got %d: %+v", len(q), q)
	}
	if q[0].Number != 1 || q[0].Held {
		t.Fatalf("first item should be the offerable issue #1, got %+v", q[0])
	}
	if q[1].Number != 2 || !q[1].Held {
		t.Fatalf("a held tracker must still surface tagged Held=true, got %+v", q[1])
	}
}
