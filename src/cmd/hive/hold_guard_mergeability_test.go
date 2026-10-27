package main

// Tests for #10437: a held PR must stay mergeable, not merely frozen. These
// exercise maintainHeldPRMergeability through enforceHoldGuard, mirroring the
// integration style of hold_guard_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/holdguard"
)

func heldPRsActionable(prs ...github.PullRequest) *github.ActionableResult {
	a := &github.ActionableResult{}
	a.PRs.Held = prs
	return a
}

func TestMaintainHeldPRMergeabilityUpdatesBehindBranchAndAdvancesBaseline(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("c2", "hive-bot", "Merge branch 'main' into feature")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	if !store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}}) {
		t.Fatal("test setup: snapshot must record")
	}

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "behind"})
	if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
		t.Fatalf("keeping a held PR mergeable must not flag it for re-review, got %v", got)
	}

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("update-branch calls for PR 7 = %d, want 1", calls)
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %+v ok=%v, want it advanced to the merged head c2", rec, ok)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("a successful hygiene push must not comment, saw %v", fake.comments)
	}
}

func TestMaintainHeldPRMergeabilityNotesUnresolvableConflictOnce(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{
		failUpdateBranch: true,
		commits:          map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan")}},
	}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "dirty"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	if len(fake.comments) != 1 {
		t.Fatalf("want one conflict notice, got %v", fake.comments)
	}
	if !strings.Contains(fake.comments[0], "conflict") {
		t.Fatalf("conflict comment = %q, want it to name the conflict", fake.comments[0])
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || !rec.ConflictNoted {
		t.Fatalf("snapshot = %+v ok=%v, want ConflictNoted set", rec, ok)
	}

	// A retried tick with the same unresolved conflict must not re-comment.
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())
	if len(fake.comments) != 1 {
		t.Fatalf("conflict notice must be deduplicated, saw %v", fake.comments)
	}

	// The same conflict on the same head and base is not asked again.
	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("update-branch calls for an unchanged conflicting head and base = %d, want 1", calls)
	}

	// The base moves and the conflict goes away; the next behind push
	// succeeds, and the stale conflict notice must clear.
	fake.mu.Lock()
	fake.failUpdateBranch = false
	fake.commits[7] = append(fake.commits[7], ghCommit("c2", "hive-bot", "Merge branch 'main' into feature"))
	fake.mu.Unlock()
	heldResolved := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b2", MergeableState: "behind"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, heldResolved, discardLogger())
	rec, ok = store.Recorded("acme/widgets", 7)
	if !ok || rec.ConflictNoted {
		t.Fatalf("snapshot = %+v ok=%v, want ConflictNoted cleared after the branch merges clean", rec, ok)
	}
	if rec.HeadSHA != "c2" {
		t.Fatalf("baseline = %q, want it advanced to c2 once the conflict clears", rec.HeadSHA)
	}
}

func TestMaintainHeldPRMergeabilitySkipsUntrackedPR(t *testing.T) {
	newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{42: {ghCommit("z1", "strategist-bot", "docs: plan")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	// No snapshot recorded for PR 42 — the guard must never touch its branch
	// before the snapshot step has pinned a baseline for it.
	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 42, HeadSHA: "z1", MergeableState: "behind"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	fake.mu.Lock()
	calls := fake.updateBranchCalls[42]
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("update-branch calls for an untracked PR = %d, want 0", calls)
	}
}

func TestMaintainHeldPRMergeabilityIgnoresCleanPR(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: "clean"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())

	fake.mu.Lock()
	calls := fake.updateBranchCalls[7]
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("update-branch calls for a clean held PR = %d, want 0", calls)
	}
}

// A held PR GitHub refuses to update because the base changed workflow files
// and the App lacks the workflows permission (projectbluefin/testsuite#991,
// retried every pass for six hours) gets one attempt per head and base: the
// refusal is recorded with its reason, one note tells a person to update the
// branch (not the conflict notice), and only a new head or base retries.
func TestMaintainHeldPRMergeabilityStopsOnWorkflowPermissionRefusal(t *testing.T) {
	for _, state := range []string{"behind", "dirty"} {
		t.Run(state, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{
				workflowRefused: true,
				commits:         map[int][]map[string]any{7: {ghCommit("c1", "strategist-bot", "docs: plan")}},
			}
			client := newHoldGuardClient(t, fake)
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", nil)

			held := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", MergeableState: state})
			for i := 0; i < 3; i++ {
				_ = enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger())
			}

			fake.mu.Lock()
			calls := fake.updateBranchCalls[7]
			comments := len(fake.comments)
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("update-branch calls for a refused head = %d, want 1", calls)
			}
			if comments != 1 {
				t.Fatalf("want one update notice for a workflows-permission refusal; saw %d comments", comments)
			}
			fake.mu.Lock()
			note := fake.comments[0]
			fake.mu.Unlock()
			if !strings.Contains(note, "workflows permission") || !strings.Contains(note, "Update branch") || strings.Contains(note, "real conflict") {
				t.Fatalf("notice = %q, want the workflows-permission reason and the Update branch step, not the conflict notice", note)
			}
			rec, ok := store.Recorded("acme/widgets", 7)
			if !ok || rec.UpdateStoppedHeadSHA != "c1" || rec.UpdateStoppedReason != holdguard.UpdateStopWorkflowPermission || rec.ConflictNoted {
				t.Fatalf("snapshot = %+v ok=%v, want an update stop at c1 for the workflows permission and no conflict noted", rec, ok)
			}

			// The base moves; the guard asks again, and the same refusal
			// does not post a second note for the same head.
			movedBase := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c1", BaseSHA: "b2", MergeableState: state})
			_ = enforceHoldGuard(context.Background(), cfg, client, client, movedBase, discardLogger())
			_ = enforceHoldGuard(context.Background(), cfg, client, client, movedBase, discardLogger())
			fake.mu.Lock()
			calls = fake.updateBranchCalls[7]
			comments = len(fake.comments)
			fake.mu.Unlock()
			if calls != 2 || comments != 1 {
				t.Fatalf("after the base moved: update-branch calls = %d, comments = %d; want 2 and 1", calls, comments)
			}

			// The head moves (a person pushed); the guard tries again.
			movedHead := heldPRsActionable(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "c2", BaseSHA: "b2", MergeableState: state})
			_ = enforceHoldGuard(context.Background(), cfg, client, client, movedHead, discardLogger())
			fake.mu.Lock()
			calls = fake.updateBranchCalls[7]
			fake.mu.Unlock()
			if calls != 3 {
				t.Fatalf("update-branch calls after the head moved = %d, want 3", calls)
			}
		})
	}
}
