package config

import (
	"fmt"
	"regexp"
	"strings"
)

// StandingApproval is one kind of change the hive owner approves in advance
// (project.standing_approvals). Text says, in the owner's words, what is
// approved; it is what the agent reads. Repos and Agents narrow where it
// applies: empty means every repo the hive serves and every lane.
type StandingApproval struct {
	ID     string   `yaml:"id" json:"id"`
	Text   string   `yaml:"text" json:"text"`
	Repos  []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	Agents []string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

const (
	// MaxStandingApprovals caps the list: each entry is rendered into every
	// issue-filing kick, so a long list is prompt weight on every lane.
	MaxStandingApprovals = 20
	// MaxStandingApprovalTextBytes caps one entry's text for the same reason.
	MaxStandingApprovalTextBytes = 2048
)

// standingApprovalIDRE is the id shape the relay flag accepts: short,
// lowercase, and safe to echo into an issue body and an audit line.
var standingApprovalIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidStandingApprovalID reports whether id has the shape an entry id must.
func ValidStandingApprovalID(id string) bool {
	return standingApprovalIDRE.MatchString(id)
}

// ValidateStandingApprovals checks the list's shape: unique well-formed ids,
// non-empty text within the size cap, no blank repo or agent names. It does
// not check repos and agents against the rest of the config: an entry that
// names a repo or lane this hive does not have simply never applies, and a
// config load must not fail on a lane a pack adds later.
func ValidateStandingApprovals(list []StandingApproval) error {
	if len(list) > MaxStandingApprovals {
		return fmt.Errorf("project.standing_approvals has %d entries, over the limit of %d", len(list), MaxStandingApprovals)
	}
	seen := map[string]bool{}
	for i, sa := range list {
		id := strings.TrimSpace(sa.ID)
		if !ValidStandingApprovalID(id) {
			return fmt.Errorf("project.standing_approvals[%d]: id %q must be lowercase letters, digits and dashes (at most 63)", i, sa.ID)
		}
		if seen[id] {
			return fmt.Errorf("project.standing_approvals[%d]: id %q is used twice", i, id)
		}
		seen[id] = true
		text := strings.TrimSpace(sa.Text)
		if text == "" {
			return fmt.Errorf("project.standing_approvals[%d] (%s): text is required", i, id)
		}
		if len([]byte(text)) > MaxStandingApprovalTextBytes {
			return fmt.Errorf("project.standing_approvals[%d] (%s): text is %d bytes, over the %d byte limit", i, id, len([]byte(text)), MaxStandingApprovalTextBytes)
		}
		for _, r := range sa.Repos {
			if strings.TrimSpace(r) == "" {
				return fmt.Errorf("project.standing_approvals[%d] (%s): repos has a blank entry", i, id)
			}
		}
		for _, a := range sa.Agents {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("project.standing_approvals[%d] (%s): agents has a blank entry", i, id)
			}
		}
	}
	return nil
}

// standingApprovalCoversAgent reports whether sa applies to agent. A replica
// (scanner-2) is the same lane as its base, so an entry naming the base
// covers it.
func (c *Config) standingApprovalCoversAgent(sa StandingApproval, agent string) bool {
	if len(sa.Agents) == 0 {
		return true
	}
	agent = strings.TrimSpace(agent)
	base := c.BaseAgentName(agent)
	for _, a := range sa.Agents {
		a = strings.TrimSpace(a)
		if strings.EqualFold(a, agent) || strings.EqualFold(a, base) {
			return true
		}
	}
	return false
}

// standingApprovalCoversRepo reports whether sa applies to repo, comparing
// org-qualified and case-folded names the way agent repo scopes do.
func (c *Config) standingApprovalCoversRepo(sa StandingApproval, repo string) bool {
	if len(sa.Repos) == 0 {
		return true
	}
	want := agentRepoKey(c.Project.Org, repo)
	if want == "" {
		return false
	}
	for _, r := range sa.Repos {
		if agentRepoKey(c.Project.Org, r) == want {
			return true
		}
	}
	return false
}

// StandingApprovalCovers reports whether the standing approval with this id
// exists and applies to agent filing in repo. It is the relay's check, so an
// unknown id, an empty repo or an out-of-scope lane all answer false.
func (c *Config) StandingApprovalCovers(id, agent, repo string) bool {
	if c == nil {
		return false
	}
	id = strings.TrimSpace(id)
	for _, sa := range c.Project.StandingApprovals {
		if strings.TrimSpace(sa.ID) != id {
			continue
		}
		return c.standingApprovalCoversAgent(sa, agent) && c.standingApprovalCoversRepo(sa, repo)
	}
	return false
}

// StandingApprovalsSection renders the entries that apply to agent as the
// text ${STANDING_APPROVALS} expands to, or "" when none do. The variable
// sits at the end of the issue-filing policies' "Park an issue" paragraph, so
// the section opens with its own paragraph break and an empty result leaves
// the paragraph exactly as it was.
func (c *Config) StandingApprovalsSection(agent string) string {
	if c == nil {
		return ""
	}
	var lines []string
	for _, sa := range c.Project.StandingApprovals {
		id := strings.TrimSpace(sa.ID)
		text := strings.Join(strings.Fields(sa.Text), " ")
		if id == "" || text == "" || !c.standingApprovalCoversAgent(sa, agent) {
			continue
		}
		line := "- `" + id + "`"
		if len(sa.Repos) > 0 {
			repos := make([]string, 0, len(sa.Repos))
			for _, r := range sa.Repos {
				repos = append(repos, qualifyAgentRepo(c.Project.Org, r))
			}
			line += " (only in " + strings.Join(repos, ", ") + ")"
		}
		lines = append(lines, line+": "+text)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n**Standing approvals (set by this hive's owner in `project.standing_approvals`).** " +
		"The owner has approved the kinds of change below in advance. When an issue you would park " +
		"with `--needs-decision` is covered, as a whole, by one entry, keep `--needs-decision` and add " +
		"`--standing-approval <id>` with that entry's id. Hive then checks the request: if the entry " +
		"covers this repository and lane, and the issue's `Risk:` line is a governance question, it " +
		"files the issue as actionable work and records the approval in the body instead of parking it; " +
		"otherwise it parks the issue as usual and the result says why. Use an entry only when it plainly " +
		"covers the whole question; when in doubt, leave the flag off and let the owner decide. " +
		"Never use one for security posture, credentials, secrets, permissions, branch protection or " +
		"rulesets, merge or trust policy, licensing, spend, data loss or irreversible changes: Hive " +
		"refuses it there.\n\n" + strings.Join(lines, "\n")
}
