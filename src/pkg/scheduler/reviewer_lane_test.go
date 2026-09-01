package scheduler

// Tests for the reviewer lane (#5480): the escalated-PR adjudication kick.
//
// The lane's safety properties are all here: escalated-only rows, the
// one-pass-ever exclusion (reviewer-passed), the per-kick cap with oldest
// first ordering, the hard ACMM gate, and the adjudication contract itself
// (REPAIR / DE-ESCALATE / RECOMMEND-CLOSE with the close authority split at
// L6). Each is a one-line edit away from an agent that closes human queues,
// so each is pinned.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
)

const reviewerFixture = `{"generated_at":"2026-09-01T00:00:00Z","ci_failing":[
  {"number":22815,"repo":"test-org/console","title":"red but NOT escalated","agent":"scanner",
   "failing_checks":["build-gate"],"excerpt":"still in the automated lane"},
  {"number":9,"repo":"test-org/console","title":"escalated split PR","agent":"scanner","escalated":true,
   "failing_checks":["build-gate","Test (chromium, shard 1)"],
   "excerpt":"ReferenceError: seedMission is not defined"},
  {"number":41,"repo":"test-org/console","title":"escalated, already adjudicated","agent":"quality","escalated":true,
   "labels":["needs-human","reviewer-passed"],"failing_checks":["go test"]},
  {"number":3,"repo":"test-org/console","title":"oldest escalated","escalated":true,
   "labels":["needs-human"],"failing_checks":["lint"]}
]}`

// Only rows with escalated=true appear; red-but-still-automated PRs stay in
// the fix lane, and rows carrying reviewer-passed are excluded forever.
func TestFormatReviewerWorkList_EscalatedOnlyAndReviewerPassedExcluded(t *testing.T) {
	out := formatReviewerWorkList([]byte(reviewerFixture))
	for _, want := range []string{
		"test-org/console#9",
		"test-org/console#3",
		"gh pr checkout 9 --repo test-org/console",
		"build-gate, Test (chromium, shard 1)",
		"ReferenceError: seedMission is not defined",
		"original author agent: scanner",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("work list missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#22815") {
		t.Error("non-escalated red PR must stay in the automated fix lane, not the reviewer's list")
	}
	if strings.Contains(out, "#41") {
		t.Error("reviewer-passed PR must be excluded: one reviewer pass per PR, ever")
	}
	// Unattributed rows surface as scanner, the fleet's primary PR creator.
	if !strings.Contains(out, "original author agent: scanner") {
		t.Errorf("unattributed escalated row must default its author agent to scanner:\n%s", out)
	}
	// Oldest first: #3 precedes #9.
	if strings.Index(out, "#3 ") > strings.Index(out, "#9 ") {
		t.Errorf("work list must be oldest first (ascending PR number):\n%s", out)
	}

	if got := formatReviewerWorkList([]byte(`{"ci_failing":[{"number":1,"repo":"o/r","title":"red"}]}`)); got != "" {
		t.Errorf("no escalated rows must yield an empty list, got:\n%s", got)
	}
	if got := formatReviewerWorkList([]byte("not json")); got != "" {
		t.Errorf("malformed file must yield an empty list, got:\n%s", got)
	}
}

// The work list is structurally capped at reviewerMaxPRsPerKick rows, oldest
// first, with the remainder summarized rather than listed.
func TestFormatReviewerWorkList_CapOldestFirst(t *testing.T) {
	var rows []string
	for _, n := range []int{50, 10, 40, 20, 30} { // deliberately out of order
		rows = append(rows, fmt.Sprintf(`{"number":%d,"repo":"o/r","title":"t%d","agent":"scanner","escalated":true}`, n, n))
	}
	data := `{"ci_failing":[` + strings.Join(rows, ",") + `]}`
	out := formatReviewerWorkList([]byte(data))

	for _, want := range []string{"o/r#10", "o/r#20", "o/r#30"} {
		if !strings.Contains(out, want) {
			t.Errorf("capped list missing oldest row %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"o/r#40", "o/r#50"} {
		if strings.Contains(out, banned) {
			t.Errorf("row %q exceeds the per-kick cap of %d:\n%s", banned, reviewerMaxPRsPerKick, out)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("… %d more escalated PRs", 2)) {
		t.Errorf("expected cap summary line for the 2 held-back rows:\n%s", out)
	}
	if strings.Index(out, "#10") > strings.Index(out, "#20") || strings.Index(out, "#20") > strings.Index(out, "#30") {
		t.Errorf("rows must render oldest first:\n%s", out)
	}
}

func reviewerTestScheduler(t *testing.T, acmmLevel int, fixture string) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if fixture != "" {
		if err := os.WriteFile(ciFailingPath, []byte(fixture), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Agents: map[string]config.AgentConfig{
			// An operator-added adjudicator: name is NOT "reviewer"; the ROLE
			// is what routes it into the lane.
			"adjudicator": {Role: "reviewer", Mode: "ISSUES_AND_PRS"},
			"scanner":     {Mode: "ISSUES_AND_PRS"},
		},
	}
	if acmmLevel > 0 {
		cfg.ACMMLevel = &acmmLevel
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// Below ACMM L5 the kick renders only the dormant notice — no work list, no
// contract — regardless of how deep the escalated queue is. This is the hard
// gate: an operator adding a reviewer-role agent to a low-trust hive must not
// acquire an agent that un-escalates human queues.
func TestBuildReviewerMessage_ACMMGateDormantBelowL5(t *testing.T) {
	for _, level := range []int{0, 1, 2, 3, 4} {
		s := reviewerTestScheduler(t, level, reviewerFixture)
		msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
		if !strings.Contains(msg, "REVIEWER LANE DORMANT") {
			t.Errorf("L%d: kick must say the lane is dormant:\n%s", level, msg)
		}
		if !strings.Contains(msg, "Stand down") {
			t.Errorf("L%d: dormant kick must order a stand-down:\n%s", level, msg)
		}
		for _, banned := range []string{"#9", "#3", "ADJUDICATION CONTRACT", "REPAIR", "gh pr checkout"} {
			if strings.Contains(msg, banned) {
				t.Errorf("L%d: dormant kick must not render %q:\n%s", level, banned, msg)
			}
		}
	}
}

// At L5+ the kick carries the work list and the full adjudication contract:
// the three exclusive verdicts, the same-branch repair rule, label mechanics,
// the cap, and the human-authored-PR and reviewer-passed invariants. Below L6
// closing is forbidden.
func TestBuildReviewerMessage_ContractAtL5(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	for _, want := range []string{
		"[agent:adjudicator]",
		"test-org/console#9",
		"REPAIR",
		"DE-ESCALATE",
		"RECOMMEND-CLOSE",
		"EXACTLY ONE verdict",
		"diff/test-count parity",
		"SAME branch",
		"Do NOT open a replacement PR",
		"--remove-label needs-human",
		"--add-label " + ReviewerPassedLabel,
		"[reviewer] recommend close:",
		"NEVER close the PR yourself",
		fmt.Sprintf("AT MOST %d PRs this kick", reviewerMaxPRsPerKick),
		"NEVER touch a human-authored PR",
		"prior reviewer pass",
		"NEVER run gh pr list",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("L5 reviewer kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "MAY then close") {
		t.Error("closing must not be offered below L6")
	}
}

// At L6, and only at L6+, the kick grants close authority after the
// recommend-close comment.
func TestBuildReviewerMessage_CloseAllowedAtL6(t *testing.T) {
	s := reviewerTestScheduler(t, 6, reviewerFixture)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msg, "MAY then close the PR yourself") {
		t.Errorf("L6 kick must grant close authority:\n%s", msg)
	}
	if strings.Contains(msg, "NEVER close the PR yourself") {
		t.Errorf("L6 kick must not simultaneously forbid closing:\n%s", msg)
	}
}

// An empty escalated queue produces a stand-down kick, not a hunt.
func TestBuildReviewerMessage_EmptyQueueStandsDown(t *testing.T) {
	s := reviewerTestScheduler(t, 5, `{"ci_failing":[{"number":1,"repo":"o/r","title":"red, not escalated"}]}`)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msg, "(none)") || !strings.Contains(msg, "stand down") {
		t.Errorf("empty queue must render (none) + stand down:\n%s", msg)
	}
	if strings.Contains(msg, "ADJUDICATION CONTRACT") {
		t.Errorf("no contract without work:\n%s", msg)
	}
}

// BuildAgentMessage routes by ROLE, not name: an operator-added agent with
// role: reviewer receives the adjudication kick through the normal resolution
// chain's hardcoded fallback, so it participates in ordinary cadence kicks
// like every other role.
func TestBuildAgentMessage_RoutesReviewerRole(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	msg := s.BuildAgentMessage("adjudicator", nil, &github.ActionableResult{})
	if !strings.Contains(msg, "[agent:adjudicator]") || !strings.Contains(msg, "ADJUDICATION CONTRACT") {
		t.Errorf("role: reviewer agent must receive the adjudication kick:\n%s", msg)
	}
	// A role-less agent must not be captured by the reviewer routing.
	scanner := s.BuildAgentMessage("scanner", nil, &github.ActionableResult{})
	if strings.Contains(scanner, "ADJUDICATION CONTRACT") {
		t.Errorf("scanner must not receive the reviewer kick:\n%s", scanner)
	}
}
