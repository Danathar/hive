package scheduler

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/policies"
)

// project.standing_approvals reach the agent through the kick as
// ${STANDING_APPROVALS}, at the end of the "Park an issue" paragraph. These
// drive the real embedded quality-full.md through the kick's own substitution.

func standingApprovalsKick(t *testing.T, list []config.StandingApproval) string {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "widgets", Repos: []string{"widgets"}, StandingApprovals: list},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"quality": {Role: "quality"}},
		},
	}
	s := New(cfg, slog.Default())
	data, err := policies.DefaultPolicies.ReadFile("defaults/quality-full.md")
	if err != nil {
		t.Fatalf("read embedded quality-full.md: %v", err)
	}
	out, _ := s.substituteTemplateWithPolicy(string(data), nil, "quality", nil)
	return out
}

func TestKick_StandingApprovalsFollowTheParkRule(t *testing.T) {
	out := standingApprovalsKick(t, []config.StandingApproval{
		{ID: "advisory-ci-checks", Text: "Adding an advisory CI job is approved in advance."},
		{ID: "scanner-only", Text: "Not for quality.", Agents: []string{"scanner"}},
	})
	if strings.Contains(out, "${STANDING_APPROVALS}") {
		t.Fatal("${STANDING_APPROVALS} was left literal in the kick")
	}
	parkAt := strings.Index(out, "Use the flag; do not name the label yourself.")
	entryAt := strings.Index(out, "- `advisory-ci-checks`: Adding an advisory CI job is approved in advance.")
	if parkAt < 0 || entryAt < 0 || entryAt < parkAt {
		t.Fatalf("the entry must follow the park rule (park@%d entry@%d):\n%s", parkAt, entryAt, out)
	}
	if !strings.Contains(out, "`--standing-approval <id>`") {
		t.Fatal("the kick must tell the agent which flag to use")
	}
	if strings.Contains(out, "scanner-only") {
		t.Fatal("an entry scoped to another lane leaked into the quality kick")
	}
}

func TestKick_StandingApprovalsUnsetLeavesTheParkRuleUnchanged(t *testing.T) {
	out := standingApprovalsKick(t, nil)
	if strings.Contains(out, "${STANDING_APPROVALS}") || strings.Contains(out, "Standing approvals") {
		t.Fatal("no standing approvals must render nothing")
	}
	if !strings.Contains(out, "Use the flag; do not name the label yourself.\n") {
		t.Fatal("with no standing approvals the park paragraph must end exactly as before")
	}
}
