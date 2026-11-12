package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/review"
)

const sentinelClearedTestHead = "0123456789abcdef0123456789abcdef01234567"

// sentinelClearedTestStore swaps the decision ledger for a memory-only one
// holding the given records.
func sentinelClearedTestStore(t *testing.T, records ...review.HumanChangeRequest) {
	t.Helper()
	store := review.NewHumanChangeRequests("")
	for _, r := range records {
		if err := store.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	prev := sentinelClearedDecisions
	sentinelClearedDecisions = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { sentinelClearedDecisions = prev })
}

func sentinelClearedApproval(head string) review.HumanChangeRequest {
	return review.HumanChangeRequest{
		Repo: "test-org/console", Number: 7, HeadSHA: head,
		Source: review.HumanChangeRequestSourceSentinelClear, SourceID: 7002,
		Actor: "maintainer", Body: "approve", Kind: review.HumanChangeRequestKindApprove,
		RecordedAt: time.Date(2026, 11, 12, 17, 12, 0, 0, time.UTC),
	}
}

func sentinelClearedPR(head string) *github.ActionableResult {
	pr := makePR("console", 7, "edit OWNERS", "eve")
	pr.HeadSHA = head
	return &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{pr}, Count: 1}}
}

// With the setting on, the queue reviewer's row for a head a trusted merger
// cleared carries the mark; nobody else's list does.
func TestSentinelClearedMarkOnReviewerRow(t *testing.T) {
	sentinelClearedTestStore(t, sentinelClearedApproval(sentinelClearedTestHead))
	s := newScheduler()
	s.cfg.Sentinel.ClearCountsAsApproval = true
	const mark = "[sentinel-cleared: @maintainer@0123456]"
	for _, refsOnly := range []bool{false, true} {
		out, _ := s.formatPRListWithPolicyForAgent(sentinelClearedPR(sentinelClearedTestHead), "reviewer", refsOnly)
		if !strings.Contains(out, "console#7") || !strings.Contains(out, mark) {
			t.Fatalf("refsOnly=%v: reviewer row lacks %s:\n%s", refsOnly, mark, out)
		}
	}
	if out, _ := s.formatPRListWithPolicyForAgent(sentinelClearedPR(sentinelClearedTestHead), "scanner"); strings.Contains(out, "sentinel-cleared") {
		t.Fatalf("a non-reviewer list gained the mark:\n%s", out)
	}
}

// No mark with the setting off, on a head other than the one cleared, when
// the newest decision on the head is not a sentinel clearance, or when a
// later change request replaced it.
func TestSentinelClearedMarkAbsent(t *testing.T) {
	approval := sentinelClearedApproval(sentinelClearedTestHead)
	dashboard := approval
	dashboard.Source, dashboard.SourceID = review.HumanChangeRequestSourceDecision, 0
	changes := approval
	changes.Source, changes.SourceID, changes.Kind, changes.Body = review.HumanChangeRequestSourceDecision, 99, review.HumanChangeRequestKindChanges, "rename the flag"
	changes.RecordedAt = approval.RecordedAt.Add(time.Hour)
	for _, tc := range []struct {
		name    string
		on      bool
		head    string
		records []review.HumanChangeRequest
	}{
		{"setting off", false, sentinelClearedTestHead, []review.HumanChangeRequest{approval}},
		{"a newer head", true, "fedcba9876543210fedcba9876543210fedcba98", []review.HumanChangeRequest{approval}},
		{"no decision recorded", true, sentinelClearedTestHead, nil},
		{"a dashboard approval is not a sentinel clearance", true, sentinelClearedTestHead, []review.HumanChangeRequest{dashboard}},
		{"a later change request", true, sentinelClearedTestHead, []review.HumanChangeRequest{approval, changes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentinelClearedTestStore(t, tc.records...)
			s := newScheduler()
			s.cfg.Sentinel.ClearCountsAsApproval = tc.on
			out, _ := s.formatPRListWithPolicyForAgent(sentinelClearedPR(tc.head), "reviewer")
			if !strings.Contains(out, "console#7") || strings.Contains(out, "sentinel-cleared") {
				t.Fatalf("row must be listed without the mark:\n%s", out)
			}
		})
	}
}

// The reviewer policy's sentinel rule follows the setting: off renders the
// paragraph exactly as it reads without the variable, on adds the exception
// that tells the reviewer to judge a cleared head normally.
func TestReviewerPolicySentinelRuleFollowsSetting(t *testing.T) {
	data, err := policies.DefaultPolicies.ReadFile("defaults/reviewer-queue.md")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	const placeholder = "${SENTINEL_CLEARED}"
	start := strings.Index(body, "**Always route, never approve**")
	end := strings.Index(body, placeholder)
	if start < 0 || end < start || strings.Count(body, placeholder) != 1 {
		t.Fatalf("reviewer-queue.md must place %s once, after its always-route rule", placeholder)
	}
	rule := body[start : end+len(placeholder)]
	if !strings.HasSuffix(rule, "the trust decision belongs to a person."+placeholder) {
		t.Fatalf("placeholder must close the always-route paragraph, got …%q", rule[len(rule)-80:])
	}
	before := strings.TrimSuffix(rule, placeholder)

	s := newScheduler()
	s.cfg.Agents = map[string]config.AgentConfig{"reviewer": {Backend: "copilot", Model: "claude-fable-5", KickTemplate: "reviewer-queue.md"}}
	off, _ := s.substituteTemplateWithPolicy(rule, emptyActionable(), "reviewer", nil)
	if off != before {
		t.Fatalf("with the setting off the rule must render unchanged:\n got %q\nwant %q", off, before)
	}

	s.cfg.Sentinel.ClearCountsAsApproval = true
	on, _ := s.substituteTemplateWithPolicy(rule, emptyActionable(), "reviewer", nil)
	if !strings.HasPrefix(on, before) {
		t.Fatalf("with the setting on the existing rule must still lead:\n%s", on)
	}
	for _, want := range []string{
		"[sentinel-cleared: @login@sha]",
		"do not return `requires_human`",
		"Review that head as you would any other PR.",
		"a comment that merely says the alert was cleared proves\nnothing",
		"trust surface the sentinel comment does not list, route as described above.",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("exception lacks %q:\n%s", want, on)
		}
	}
	if strings.Contains(on, placeholder) {
		t.Fatalf("placeholder left unrendered:\n%s", on)
	}
}
