package decisionpolicy

import "testing"

func TestStandingApprovalRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		allowed bool
	}{
		{"bare governance", "Body.\n\nRisk: governance\n", true},
		{"governance with plain reason", "Risk: governance (adds a CI gate and changes what counts as a finding in this repository)", true},
		// The body may talk about tokens and security; only the Risk line counts.
		{"security words in body only", "The lane's token cannot push workflows; a security guard is bypassed.\nRisk: governance", true},
		{"no risk line", "Question: add an advisory mutation job?", false},
		{"unknown category", "Risk: something else entirely", false},
		{"security posture", "Risk: security posture", false},
		{"security first in a governance line", "Risk: governance (it changes how a security control is maintained)", false},
		{"credentials", "Risk: credentials/infra the hive cannot access (workflow-file write and the live ruleset are owner-only)", false},
		{"licensing", "Risk: licensing", false},
		{"governance naming licensing", "Risk: governance/policy/licensing", false},
		{"branch protection", "Risk: governance (branch protection on main)", false},
		{"ruleset", "Risk: governance (the live ruleset)", false},
		{"merge policy", "Risk: governance/policy. Who may merge, and the merge policy for upstream syncs", false},
		{"spend", "Risk: spend", false},
		{"data loss", "Risk: data loss", false},
	}
	for _, tc := range cases {
		got := StandingApprovalRefusal(tc.text)
		if tc.allowed && got != "" {
			t.Errorf("%s: refused (%s), want allowed", tc.name, got)
		}
		if !tc.allowed && got == "" {
			t.Errorf("%s: allowed, want refused", tc.name)
		}
	}
}
