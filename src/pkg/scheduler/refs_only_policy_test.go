package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
)

func TestRefsOnlyIssueListRunsPolicyAndAudit(t *testing.T) {
	s := newSchedulerWithIoscanFailMode(true, "closed")
	fake := &fakeInjectionClassifier{score: ioscan.InjectionScore{
		Score: 0.91, Category: "critical", Rationale: "asks agent to ignore operator",
	}}
	s.SetClassifier(fake, ioscan.Thresholds{Warn: 0.5, Block: 0.8})
	calls := collectClassifierAudit(s)

	out, failClosed := s.formatIssueListWithPolicyForAgent([]github.Issue{{
		Repo: "test-org/console", Number: 7, Title: blockingTitle, Labels: []string{"kind/bug"},
	}}, true)
	if !failClosed {
		t.Fatal("refs-only issue list did not return the closed policy result")
	}
	if !strings.Contains(out, "test-org/console#7") || strings.Contains(out, blockingTitle) {
		t.Fatalf("refs-only output = %q, want ref without title", out)
	}
	if got := classifierEntries(*calls); len(got) == 0 {
		t.Fatalf("classifier audit entries = 0, want at least one (all: %+v)", *calls)
	}
}

func TestRefsOnlyPRListKeepsForkAnnotation(t *testing.T) {
	s := newSchedulerWithIoscanFailMode(true, "open")
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{{
		Repo: "test-org/console", Number: 8, Title: "fix docs", Author: "alice", FromFork: true, HeadRepo: "alice/console",
	}}}}

	out, failClosed := s.formatPRListWithPolicyForAgent(actionable, "scanner", true)
	if failClosed {
		t.Fatalf("refs-only PR list failClosed = true, output: %s", out)
	}
	for _, want := range []string{"test-org/console#8", "[fork: alice/console — comment only, cannot push]", "mergeable_state="} {
		if !strings.Contains(out, want) {
			t.Fatalf("refs-only PR output missing %q: %s", want, out)
		}
	}

	msg := s.buildScannerMessage(nil, actionable, true)
	if !strings.Contains(msg, "[fork: alice/console — comment only, cannot push]") {
		t.Fatalf("scanner refs-only output missing fork annotation: %s", msg)
	}
}
