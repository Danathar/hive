package escalation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func obs(repo string, num int, sha string, red bool) Observation {
	return Observation{Repo: repo, Number: num, HeadSHA: sha, Red: red, Excerpt: "ReferenceError: seedMission is not defined"}
}

// obsLabeled is obs with the PR's current forge labels attached, as the
// enumeration pass supplies them for reviewer-verdict reconciliation.
func obsLabeled(repo string, num int, sha string, red bool, labels ...string) Observation {
	o := obs(repo, num, sha, red)
	o.Labels = labels
	return o
}

func TestSweep_EscalatesAtThresholdOfDistinctRedSHAs(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// Attempt 1: red.
	r := s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 || got.NewlyEscala {
		t.Fatalf("attempt 1: got %+v", got)
	}
	// Same SHA re-observed: NOT a new attempt.
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 {
		t.Fatalf("same sha must not increment: got %+v", got)
	}
	// Attempts 2 and 3: new SHAs, still red — threshold crossed on 3.
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	got := r[Key("org/repo", 7)]
	if got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("attempt 3 must escalate: got %+v", got)
	}

	// After MarkEscalated, further sweeps must not re-fire.
	s.MarkEscalated("org/repo", 7)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	got = r[Key("org/repo", 7)]
	if got.NewlyEscala || !got.Escalated {
		t.Fatalf("escalation must fire once: got %+v", got)
	}
}

func TestSweep_GreenResetsAndAbsencePrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaks.json")
	s := Load(path)

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	// Goes green: history forgotten.
	s.Sweep([]Observation{obs("org/repo", 7, "sha3", false)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("green must reset, got %d attempts", n)
	}
	// Red again, then vanishes from the open set (merged/closed): pruned.
	s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 8, "shaX", true)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("absent PR must be pruned, got %d attempts", n)
	}

	// Persistence across Load.
	s2 := Load(path)
	if n := s2.Attempts("org/repo", 8); n != 1 {
		t.Fatalf("ledger must persist, got %d attempts for #8", n)
	}
}

func TestExcerpt_ReturnsStoredEvidenceOrEmpty(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := s.Excerpt("org/repo", 7); got != "ReferenceError: seedMission is not defined" {
		t.Fatalf("Excerpt = %q, want the stored CI evidence", got)
	}
	if got := s.Excerpt("org/repo", 8); got != "" {
		t.Fatalf("Excerpt for an unknown PR = %q, want empty", got)
	}
}

func TestCommentBody_LeadsWithEvidence(t *testing.T) {
	body := CommentBody(3, []string{"Coverage Suite", "build-gate"}, "ReferenceError: seedMission is not defined")
	for _, want := range []string{"3 distinct fix attempts", "Coverage Suite", "seedMission is not defined", NeedsHumanLabel} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
}

// --- Staleness + re-engagement (red-PR re-engagement machinery) ---

// mkClock returns a controllable clock and a pointer to advance it.
func mkClock(start time.Time) (func() time.Time, *time.Time) {
	cur := start
	return func() time.Time { return cur }, &cur
}

func TestStaleRed_FreshRedNotStale_AgedRedStale(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, cur := mkClock(time.Unix(1_000_000, 0).UTC())
	s.SetClock(clock)

	// A red PR just observed: NOT stale (clock has not advanced).
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	if s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a just-seen red SHA must not be stale")
	}
	// Advance past the threshold on the SAME SHA: now stale.
	*cur = cur.Add(RedPRStaleAfter + time.Second)
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	if !s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a red SHA unchanged past RedPRStaleAfter must be stale")
	}
	// A NEW red SHA resets the clock: not stale again (agent pushed a fix).
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha2", Red: true}})
	if s.StaleRed("org/repo", 7, "sha2") {
		t.Fatal("a freshly-changed red SHA must not be stale")
	}
	// StaleRed for a SHA that is not the tracked current one is never stale.
	if s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("stale check must key off the CURRENT red SHA only")
	}
}

func TestStaleRed_HealthyPRNeverStale(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, cur := mkClock(time.Unix(2_000_000, 0).UTC())
	s.SetClock(clock)

	// Pending/green PR: ObserveRed with Red:false must leave no staleness record.
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "greenSHA", Red: false}})
	*cur = cur.Add(2 * RedPRStaleAfter)
	if s.StaleRed("org/repo", 9, "greenSHA") {
		t.Fatal("a healthy PR must never be reported stale")
	}
	// A PR that was red then went green: staleness record cleared.
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "redSHA", Red: true}})
	*cur = cur.Add(2 * RedPRStaleAfter)
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "redSHA", Red: false}})
	if s.StaleRed("org/repo", 9, "redSHA") {
		t.Fatal("a PR that recovered to green must not stay stale")
	}
}

func TestTryReEngage_CapHaltsPermanentlyRedPR(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, _ := mkClock(time.Unix(3_000_000, 0).UTC())
	s.SetClock(clock)

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 5, HeadSHA: "stuck", Red: true}})
	// Exactly MaxReEngagements dispatches are allowed for one unchanged red SHA.
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 5, "stuck") {
			t.Fatalf("re-engagement %d must be allowed (cap is %d)", i+1, MaxReEngagements)
		}
	}
	if s.TryReEngage("org/repo", 5, "stuck") {
		t.Fatal("re-engagement past the cap must be refused — a never-moving red PR must stop being nudged")
	}
	if got := s.ReEngagements("org/repo", 5); got != MaxReEngagements {
		t.Fatalf("re-engagement count = %d, want %d", got, MaxReEngagements)
	}
	// A NEW red SHA (agent pushed a fix, still red) resets the cap.
	if !s.TryReEngage("org/repo", 5, "moved") {
		t.Fatal("a changed red SHA must reset the re-engagement cap")
	}
	if got := s.ReEngagements("org/repo", 5); got != 1 {
		t.Fatalf("after SHA change re-engagement count = %d, want 1", got)
	}
}

func TestTryReEngage_EmptySHAReusesTrackedSHA(t *testing.T) {
	// The merge-watcher hook passes an empty head SHA (it does not re-fetch the
	// head); the store must reuse the SHA last observed and NOT reset the cap.
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, _ := mkClock(time.Unix(4_000_000, 0).UTC())
	s.SetClock(clock)

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 3, HeadSHA: "abc", Red: true}})
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 3, "") {
			t.Fatalf("empty-SHA re-engagement %d must be allowed", i+1)
		}
	}
	if s.TryReEngage("org/repo", 3, "") {
		t.Fatal("empty-SHA re-engagement must respect the cap on the tracked SHA")
	}
}

// A PR whose red SHA NEVER changes (fix attempts are not even pushed — e.g.
// agents lost write credentials) must still escalate once its re-engagement
// budget is exhausted. Without this, the distinct-SHA count never advances and
// the PR is nudged forever without ever reaching a human (kubestellar/console,
// 2026-08-22: eight red PRs re-engaged every cycle for 15h with zero pushes).
func TestSweep_EscalatesWhenReEngagementBudgetExhaustedOnUnchangedSHA(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// One red SHA, observed; re-engage to the cap without the SHA ever moving.
	s.Sweep([]Observation{obs("org/repo", 9, "frozen", true)}, 3)
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 9, "frozen") {
			t.Fatalf("re-engage %d should be allowed", i+1)
		}
	}
	if s.TryReEngage("org/repo", 9, "frozen") {
		t.Fatal("cap must halt further re-engagements")
	}

	// Next sweep: still the same red SHA, one distinct attempt — but the
	// budget is exhausted, so escalation must fire now.
	r := s.Sweep([]Observation{obs("org/repo", 9, "frozen", true)}, 3)
	got := r[Key("org/repo", 9)]
	if got.Attempts != 1 || !got.NewlyEscala {
		t.Fatalf("exhausted budget on unchanged SHA must escalate: got %+v", got)
	}

	// A pushed fix (new SHA) resets the budget — a freshly-moving PR must NOT
	// be treated as exhausted.
	s2 := Load(filepath.Join(t.TempDir(), "s2.json"))
	s2.Sweep([]Observation{obs("org/repo", 11, "a", true)}, 3)
	for i := 0; i < MaxReEngagements; i++ {
		s2.TryReEngage("org/repo", 11, "a")
	}
	r = s2.Sweep([]Observation{obs("org/repo", 11, "b", true)}, 3) // branch moved
	got = r[Key("org/repo", 11)]
	if got.NewlyEscala {
		t.Fatalf("new SHA resets the budget; must not escalate yet: got %+v", got)
	}
}

// Machinery amnesty: entries escalated under an older fix-dispatch generation
// (pre-#4828 kicks carried no CI evidence and most attempts produced no
// commit) get ONE fresh budget under the current generation — un-escalated,
// counters cleared, distinct-SHA ledger restarted — instead of staying
// human-parked forever. Entries already at the current generation keep their
// state untouched.
func TestSweep_MachineryAmnestyReleasesOldGenerationEscalations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streaks.json")
	s := Load(path)

	// Simulate a generation-1 entry: escalated, budget exhausted, full ledger.
	key := Key("org/repo", 9)
	s.mu.Lock()
	s.entries[key] = &Entry{
		RedSHAs:       []string{"a", "b", "c"},
		Escalated:     true,
		CurRedSHA:     "c",
		ReEngagements: MaxReEngagements,
		Machinery:     1,
	}
	s.mu.Unlock()

	// Next sweep under generation 2: amnesty fires — not escalated, ledger
	// restarted at the observed SHA only, and it must NOT immediately
	// re-escalate off the old ledger.
	r := s.Sweep([]Observation{obs("org/repo", 9, "c", true)}, 3)
	got := r[key]
	if got.NewlyEscala {
		t.Fatalf("amnestied entry must not re-escalate on the old ledger: %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("ledger must restart: got %+v", got)
	}
	s.mu.Lock()
	stillEscalated := s.entries[key].Escalated
	s.mu.Unlock()
	if stillEscalated {
		t.Fatal("entry must be un-escalated after amnesty")
	}
	// Re-engagement budget is fresh.
	if !s.TryReEngage("org/repo", 9, "c") {
		t.Fatal("amnestied entry must have a fresh re-engagement budget")
	}
	// The amnesty fires ONCE: exhausting the fresh budget escalates again.
	for i := 0; i < MaxReEngagements; i++ {
		s.TryReEngage("org/repo", 9, "c")
	}
	if s.TryReEngage("org/repo", 9, "c") {
		t.Fatal("cap must hold at the current generation — amnesty is one-shot")
	}
}

// escalate drives a fresh entry for repo#num across three distinct red SHAs
// and marks the escalation side effects fired, leaving it Escalated under the
// CURRENT machinery generation.
func escalate(t *testing.T, s *Store, repo string, num int) {
	t.Helper()
	for _, sha := range []string{"e1", "e2", "e3"} {
		s.Sweep([]Observation{obs(repo, num, sha, true)}, 3)
	}
	s.MarkEscalated(repo, num)
}

// Reviewer-verdict reconciliation (#5511, gap G1): the reviewer lane's
// REPAIR/DE-ESCALATE verdict is label edits only (needs-human removed,
// reviewer-passed added), usually via direct gh the hub never sees. Sweep must
// sync that verdict into the ledger — reset the entry like amnesty does — or
// a reviewer fix that goes red again is orphaned: excluded from the fix lane,
// the reaper, the reviewer lane, AND the needs-human queue, forever.
func TestSweep_ReviewerPassResetsEscalatedEntry(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	escalate(t, s, "org/repo", 7)

	// The reviewer repaired and relabeled; the pushed fix is red again. The
	// entry must be reset: un-escalated, distinct-SHA ledger restarted at the
	// new red SHA, back in the automated lane.
	r := s.Sweep([]Observation{obsLabeled("org/repo", 7, "fix1", true, ReviewerPassedLabel)}, 3)
	got := r[Key("org/repo", 7)]
	if got.Escalated || got.NewlyEscala {
		t.Fatalf("reviewer pass must un-escalate the ledger entry: got %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("reviewer pass must restart the distinct-SHA ledger: got %+v", got)
	}
	// The reset entry has a fresh re-engagement budget (it is a normal
	// automated-lane entry again).
	if !s.TryReEngage("org/repo", 7, "fix1") {
		t.Fatal("reset entry must be re-engageable")
	}

	// Re-escalation happens only on fresh red evidence: two more distinct red
	// SHAs (labels unchanged — the reset must NOT repeat while un-escalated,
	// or the count could never reach the threshold).
	s.Sweep([]Observation{obsLabeled("org/repo", 7, "fix2", true, ReviewerPassedLabel)}, 3)
	r = s.Sweep([]Observation{obsLabeled("org/repo", 7, "fix3", true, ReviewerPassedLabel)}, 3)
	got = r[Key("org/repo", 7)]
	if got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("three fresh red SHAs after the reset must re-escalate: got %+v", got)
	}
}

// The reset must NOT fire while needs-human is still present (the reviewer has
// not delivered a verdict — e.g. a partially-applied label edit) or when the
// labels carry no reviewer verdict at all.
func TestSweep_ReviewerResetRequiresVerdictLabels(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// needs-human still present alongside reviewer-passed: no reset.
	escalate(t, s, "org/repo", 8)
	r := s.Sweep([]Observation{obsLabeled("org/repo", 8, "x1", true, NeedsHumanLabel, ReviewerPassedLabel)}, 3)
	if got := r[Key("org/repo", 8)]; !got.Escalated {
		t.Fatalf("needs-human still present must keep the entry escalated: got %+v", got)
	}

	// No reviewer verdict labels at all: no reset.
	escalate(t, s, "org/repo", 9)
	r = s.Sweep([]Observation{obsLabeled("org/repo", 9, "y1", true, NeedsHumanLabel)}, 3)
	if got := r[Key("org/repo", 9)]; !got.Escalated {
		t.Fatalf("an escalated entry without a reviewer verdict must stay escalated: got %+v", got)
	}
}

// TryReEngage must refuse escalated entries (#5511, gap G3): the merge-request
// watcher's terminal path calls it without consulting the escalated set, and
// without this guard it burns re-engagement budget — and logs "re-engaged fix
// loop" — for a PR the fix loop is standing down from.
func TestTryReEngage_RefusesEscalatedEntry(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	escalate(t, s, "org/repo", 5)

	if s.TryReEngage("org/repo", 5, "e3") {
		t.Fatal("an escalated entry must not be re-engaged")
	}
	if got := s.ReEngagements("org/repo", 5); got != 0 {
		t.Fatalf("refused re-engage must not burn budget: count = %d, want 0", got)
	}
	// Empty-SHA path (the merge watcher's actual call shape) too.
	if s.TryReEngage("org/repo", 5, "") {
		t.Fatal("an escalated entry must not be re-engaged via the empty-SHA path")
	}

	// Machinery amnesty still wins: an OLD-generation escalated entry is
	// un-escalated by the amnesty block and gets its fresh budget.
	key := Key("org/repo", 6)
	s.mu.Lock()
	s.entries[key] = &Entry{RedSHAs: []string{"a", "b", "c"}, Escalated: true, CurRedSHA: "c", Machinery: 1}
	s.mu.Unlock()
	if !s.TryReEngage("org/repo", 6, "c") {
		t.Fatal("amnesty must still release an older-generation escalated entry")
	}
}

// pendingObs is a CI-still-running observation (Red=false, Pending=true), as
// runEscalationSweep supplies it for every fresh push's pending window.
func pendingObs(repo string, num int, sha string) Observation {
	return Observation{Repo: repo, Number: num, HeadSHA: sha, Pending: true}
}

func TestSweep_PendingObservationPreservesAttemptHistory(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// Two failed attempts on the books.
	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)

	// A third fix is pushed; the sweep catches it mid-CI-window. Before the
	// #5617 G2 fix this took the green branch and wiped the entry, restarting
	// the distinct-SHA count and making the breaker probabilistic.
	r := s.Sweep([]Observation{pendingObs("org/repo", 7, "sha3")}, 3)
	got := r[Key("org/repo", 7)]
	if got.Attempts != 2 || got.NewlyEscala {
		t.Fatalf("pending must report existing history without escalating: got %+v", got)
	}
	if s.Attempts("org/repo", 7) != 2 {
		t.Fatalf("pending observation must not wipe the ledger: attempts=%d", s.Attempts("org/repo", 7))
	}

	// The pending SHA resolves red: threshold crossed on attempt 3.
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("attempt 3 after a pending window must escalate: got %+v", got)
	}

	// Green still clears, pending or not before it.
	s.Sweep([]Observation{obs("org/repo", 7, "sha4", false)}, 3)
	if s.Attempts("org/repo", 7) != 0 {
		t.Fatal("green must still forget the history entirely")
	}
}

func TestSweep_PendingKeepsEscalatedViewStable(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	for _, sha := range []string{"sha1", "sha2", "sha3"} {
		s.Sweep([]Observation{obs("org/repo", 7, sha, true)}, 3)
	}
	s.MarkEscalated("org/repo", 7)

	// A repair push's pending window must not drop the PR out of the
	// escalatedPRs view the reaper and ci-failing.json consume same-tick.
	r := s.Sweep([]Observation{pendingObs("org/repo", 7, "sha4")}, 3)
	got := r[Key("org/repo", 7)]
	if !got.Escalated || got.NewlyEscala {
		t.Fatalf("pending must keep reporting Escalated without re-firing: got %+v", got)
	}
}

func TestObserveRed_PendingDoesNotClearStalenessClock(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, cur := mkClock(time.Unix(3_000_000, 0).UTC())
	s.SetClock(clock)

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	s.TryReEngage("org/repo", 7, "sha1")
	*cur = cur.Add(RedPRStaleAfter + time.Second)

	// A pending re-observation (e.g. a re-run in flight) must leave both the
	// staleness clock and the re-engagement counter untouched.
	s.ObserveRed([]Observation{pendingObs("org/repo", 7, "sha1")})
	if !s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("pending observation must not reset the staleness clock")
	}
	if s.ReEngagements("org/repo", 7) != 1 {
		t.Fatalf("pending observation must not reset re-engagements: got %d", s.ReEngagements("org/repo", 7))
	}

	// Green still clears the record.
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: false}})
	if s.StaleRed("org/repo", 7, "sha1") || s.ReEngagements("org/repo", 7) != 0 {
		t.Fatal("green must still clear the staleness/re-engagement record")
	}
}
