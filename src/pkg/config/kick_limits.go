package config

// Defaults for KickLimitsConfig. The issue default is the historical
// constant; the PR default keeps the fully-expanded prompt near the budget
// documented in pkg/dashboard/prompt_history.go (~120 B per PR line).
const (
	DefaultMaxIssuesPerKick = 100
	DefaultMaxPRsPerKick    = 50
)

// KickLimitsConfig is `governor.kick_limits`: how many items each list in a
// kick prompt may carry. Zero or absent means the default; a negative value is
// treated as the default too (a cap of "none" is exactly the failure this
// exists to prevent). When a list is cut, the prompt says so with an explicit
// "… and N more" line so the agent knows the list is partial.
type KickLimitsConfig struct {
	// MaxIssues caps every issue list in a kick (work list, lane lists, held
	// PR list). Default DefaultMaxIssuesPerKick.
	MaxIssues int `yaml:"max_issues,omitempty" json:"max_issues,omitempty"`
	// MaxPRs caps every PR list in a kick (actionable PRs, stale drafts,
	// merge-eligible, CI-failing). Default DefaultMaxPRsPerKick.
	MaxPRs int `yaml:"max_prs,omitempty" json:"max_prs,omitempty"`
}

// IssuesPerKick returns max_issues with the default applied.
func (k KickLimitsConfig) IssuesPerKick() int {
	if k.MaxIssues <= 0 {
		return DefaultMaxIssuesPerKick
	}
	return k.MaxIssues
}

// PRsPerKick returns max_prs with the default applied.
func (k KickLimitsConfig) PRsPerKick() int {
	if k.MaxPRs <= 0 {
		return DefaultMaxPRsPerKick
	}
	return k.MaxPRs
}
