package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func standingApprovalsTestConfig() *Config {
	c := &Config{
		Project: ProjectConfig{
			Org:   "Acme",
			Repos: []string{"akmods", "tududi"},
			StandingApprovals: []StandingApproval{
				{ID: "advisory-ci-checks", Text: "Adding an advisory CI job\n  is approved."},
				{ID: "test-only-rows", Text: "Test-only rows.", Repos: []string{"tududi"}, Agents: []string{"sec-check"}},
			},
		},
	}
	c.Agents = map[string]AgentConfig{
		"sec-check":   {},
		"sec-check-2": {ReplicaOf: "sec-check"},
		"quality":     {},
	}
	return c
}

func TestStandingApprovals_LoadFromYAML(t *testing.T) {
	t.Parallel()
	var p ProjectConfig
	src := `
standing_approvals:
  - id: advisory-ci-checks
    text: >-
      Adding a CI job that is advisory only is approved in advance.
  - id: test-only-rows
    text: Test-only rows.
    repos: [akmods, tududi]
    agents: [quality]
`
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.StandingApprovals) != 2 {
		t.Fatalf("got %d entries, want 2", len(p.StandingApprovals))
	}
	got := p.StandingApprovals[1]
	if got.ID != "test-only-rows" || len(got.Repos) != 2 || got.Agents[0] != "quality" {
		t.Fatalf("second entry = %+v", got)
	}
	if err := ValidateStandingApprovals(p.StandingApprovals); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
}

func TestValidateStandingApprovals(t *testing.T) {
	t.Parallel()
	ok := StandingApproval{ID: "a", Text: "x"}
	cases := []struct {
		name string
		list []StandingApproval
		want string
	}{
		{"empty list", nil, ""},
		{"bad id", []StandingApproval{{ID: "Bad ID", Text: "x"}}, "must be lowercase"},
		{"blank id", []StandingApproval{{ID: "", Text: "x"}}, "must be lowercase"},
		{"duplicate id", []StandingApproval{ok, ok}, "used twice"},
		{"blank text", []StandingApproval{{ID: "a", Text: "  "}}, "text is required"},
		{"oversize text", []StandingApproval{{ID: "a", Text: strings.Repeat("x", MaxStandingApprovalTextBytes+1)}}, "byte limit"},
		{"blank repo", []StandingApproval{{ID: "a", Text: "x", Repos: []string{" "}}}, "repos has a blank"},
		{"blank agent", []StandingApproval{{ID: "a", Text: "x", Agents: []string{""}}}, "agents has a blank"},
		{"too many", make([]StandingApproval, MaxStandingApprovals+1), "over the limit"},
	}
	for _, tc := range cases {
		err := ValidateStandingApprovals(tc.list)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateRejectsBadStandingApprovals(t *testing.T) {
	t.Parallel()
	c := standingApprovalsTestConfig()
	c.Project.StandingApprovals = append(c.Project.StandingApprovals, StandingApproval{ID: "advisory-ci-checks", Text: "dup"})
	if err := c.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("Validate = %v, want the duplicate id refused", err)
	}
}

func TestStandingApprovalCovers(t *testing.T) {
	t.Parallel()
	c := standingApprovalsTestConfig()
	cases := []struct {
		id, agent, repo string
		want            bool
	}{
		{"advisory-ci-checks", "quality", "Acme/akmods", true},
		{"advisory-ci-checks", "anyone", "other-org/x", true},
		{"test-only-rows", "sec-check", "Acme/tududi", true},
		{"test-only-rows", "sec-check", "acme/TUDUDI", true},   // GitHub names are case-insensitive
		{"test-only-rows", "sec-check-2", "Acme/tududi", true}, // a replica is its base lane
		{"test-only-rows", "quality", "Acme/tududi", false},    // wrong lane
		{"test-only-rows", "sec-check", "Acme/akmods", false},  // wrong repo
		{"test-only-rows", "sec-check", "", false},
		{"no-such-entry", "quality", "Acme/akmods", false},
		{"", "quality", "Acme/akmods", false},
	}
	for _, tc := range cases {
		if got := c.StandingApprovalCovers(tc.id, tc.agent, tc.repo); got != tc.want {
			t.Errorf("Covers(%q, %q, %q) = %v, want %v", tc.id, tc.agent, tc.repo, got, tc.want)
		}
	}
	var nilCfg *Config
	if nilCfg.StandingApprovalCovers("advisory-ci-checks", "quality", "Acme/akmods") {
		t.Error("nil config must cover nothing")
	}
}

func TestStandingApprovalsSection(t *testing.T) {
	t.Parallel()
	c := standingApprovalsTestConfig()

	quality := c.StandingApprovalsSection("quality")
	if !strings.HasPrefix(quality, "\n\n**Standing approvals") {
		t.Fatalf("section must open with its own paragraph break; got %q", quality)
	}
	for _, want := range []string{"`--standing-approval <id>`", "`Risk:` line", "- `advisory-ci-checks`: Adding an advisory CI job is approved."} {
		if !strings.Contains(quality, want) {
			t.Errorf("quality section lacks %q:\n%s", want, quality)
		}
	}
	if strings.Contains(quality, "test-only-rows") {
		t.Errorf("quality section lists an entry scoped to sec-check:\n%s", quality)
	}

	sec := c.StandingApprovalsSection("sec-check-2")
	if !strings.Contains(sec, "- `test-only-rows` (only in Acme/tududi): Test-only rows.") {
		t.Errorf("replica section lacks its base lane's scoped entry:\n%s", sec)
	}

	c.Project.StandingApprovals = nil
	if got := c.StandingApprovalsSection("quality"); got != "" {
		t.Fatalf("empty list must render nothing; got %q", got)
	}
}
