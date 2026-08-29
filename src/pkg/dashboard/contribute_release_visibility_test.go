package dashboard

import (
	"strings"
	"testing"
)

// contribute_release_visibility_test.go pins the fix for kubestellar/hive#5097.
//
// A task that is RELEASED rather than finished — the socket dropped, or the
// relay asked for new work while still holding one — left no trace in the
// activity feed. The issue showed a "picked up" entry with no terminal event
// ever following it, which reads exactly like an issue nobody touched.
//
// Measured on a live hub: a contributor restarting its relay four times in ten
// minutes touched four different issues (#5056, #5058, #5057, #5055) and
// completed none. Three were abandoned mid-implementation and the hub's own
// history recorded nothing for any of them. "This contributor keeps dropping
// work" was inferable only by noticing an absence.
//
// The verb matters as much as the entry. #4260 established that a dropped socket
// is NOT a failure of the work — booking it as one is what turned three dropped
// sockets on one issue into a quarantine of an issue nobody had failed. These
// entries say "released", and the tests hold them to it.

// TestTaskDescOfMatchesPickupFormatting keeps a released task describable the
// same way its own pickup was, so the two line up in the feed instead of the
// release being phrased differently from the entry it closes out.
func TestTaskDescOfMatchesPickupFormatting(t *testing.T) {
	task := &WSTaskAssign{
		TaskID: "t-1",
		Kind:   "issue",
		Repo:   "kubestellar/hive",
		Number: 5061,
		Title:  "✨ tui T12: poll loop",
	}
	got := taskDescOf(task)
	want := "issue kubestellar/hive#5061: ✨ tui T12: poll loop"
	if got != want {
		t.Errorf("taskDescOf() = %q, want %q (the same shape as the picked-up entry)", got, want)
	}
}

// TestTaskDescOfHandlesSyntheticAndNilTasks covers the two inputs that have no
// issue to name. A synthetic pr-review task carries Number == 0 and must not
// render as "#0", which would read as a real issue and collide with every other
// numberless task in the feed.
func TestTaskDescOfHandlesSyntheticAndNilTasks(t *testing.T) {
	synthetic := &WSTaskAssign{TaskID: "pr-review-1730", Kind: "review", Repo: "kubestellar/hive"}
	if got := taskDescOf(synthetic); got != "pr-review-1730" {
		t.Errorf("a synthetic task should fall back to its id, got %q", got)
	}
	if strings.Contains(taskDescOf(synthetic), "#0") {
		t.Error("a numberless task must never render as issue #0")
	}
	if got := taskDescOf(nil); got != "" {
		t.Errorf("taskDescOf(nil) = %q, want empty", got)
	}
}

// TestReleaseActivityIsNotRecordedAsAFailure guards the verb choice against a
// well-meaning future edit. A release and a failure have different consequences
// downstream — the failure path increments the consecutive-failure counter that
// quarantines an issue — so the two must stay distinguishable in the feed as
// well as in the ledger.
func TestReleaseActivityIsNotRecordedAsAFailure(t *testing.T) {
	src := fileSource(t, "src/pkg/dashboard/contribute_ws.go")

	for _, want := range []string{
		`"released: connection lost"`,
		`"released: gave the task back"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the release activity verb %s is missing — an abandoned task would be invisible again", want)
		}
	}
	// Both release sites must describe the task; an entry with no task attached
	// says a release happened but not of what, which is barely better than
	// silence.
	if strings.Count(src, "taskDescOf(") < 3 {
		t.Error("a release entry must name the task it released")
	}
}
