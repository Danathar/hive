package apiproxy

import (
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// allForgeModes is every ACMM tier, lowest first, so the hard-deny assertions
// below cover a future fifth mode automatically.
var allForgeModes = []agent.AgentMode{
	agent.ModeAdvisory,
	agent.ModeIssuesOnly,
	agent.ModeIssuesAndPRs,
	agent.ModeIssuesPRsMerge,
}

// TestForgeRulesHardDenyAllModes verifies the apiproxy wrappers surface the
// shared crux rule table: direct MR/PR create and merge are denied on every
// forge at every autonomy level, with an agent-facing directive.
func TestForgeRulesHardDenyAllModes(t *testing.T) {
	cases := []struct{ forge, method, path string }{
		{"github", "POST", "/repos/org/repo/pulls"},
		{"github", "PUT", "/repos/org/repo/pulls/42/merge"},
		{"gitlab", "POST", "/api/v4/projects/org%2Frepo/merge_requests"},
		{"gitlab", "PUT", "/api/v4/projects/org%2Frepo/merge_requests/42/merge"},
		{"gitea", "POST", "/api/v1/repos/org/repo/pulls"},
		{"gitea", "POST", "/api/v1/repos/org/repo/pulls/42/merge"},
	}
	for _, tc := range cases {
		msg, denied := DeniedMessage(tc.method, tc.path)
		if !denied || msg == "" {
			t.Errorf("%s %s %s: expected hard-deny with directive, got denied=%v msg=%q", tc.forge, tc.method, tc.path, denied, msg)
		}
		for _, mode := range allForgeModes {
			if AllowedByMode(mode, tc.method, tc.path) {
				t.Errorf("%s %s %s: allowed at mode %v, want deny", tc.forge, tc.method, tc.path, mode)
			}
			if AllowedByModeCaps(mode, agent.AgentCapabilities{Converse: true}, tc.method, tc.path) {
				t.Errorf("%s %s %s: allowed with caps at mode %v, want deny", tc.forge, tc.method, tc.path, mode)
			}
		}
	}
}

// TestForgeRulesIssueCreateByMode verifies the wrappers apply the same minimum
// mode to the equivalent issue-create route on every forge.
func TestForgeRulesIssueCreateByMode(t *testing.T) {
	paths := map[string]string{
		"github": "/repos/org/repo/issues",
		"gitlab": "/api/v4/projects/org%2Frepo/issues",
		"gitea":  "/api/v1/repos/org/repo/issues",
	}
	for forge, path := range paths {
		if AllowedByMode(agent.ModeAdvisory, "POST", path) {
			t.Errorf("%s: advisory mode may not create issues", forge)
		}
		if !AllowedByMode(agent.ModeIssuesOnly, "POST", path) {
			t.Errorf("%s: issues-only mode should create issues", forge)
		}
		if _, denied := DeniedMessage("POST", path); denied {
			t.Errorf("%s: issue creation must not be hard-denied", forge)
		}
	}
}
