package config

import (
	"fmt"
	"strings"
)

// ACMM issue-tracker choices for governor.acmm.issue_tracker and the
// per-request `tracker` override on POST /api/acmm/issue.
const (
	// ACMMIssueTrackerGitHub files ACMM gap issues on the repo's GitHub
	// Issues — the historical and default behavior.
	ACMMIssueTrackerGitHub = "github"
	// ACMMIssueTrackerWorkSource files ACMM gap issues where the hive's
	// backlog lives (governor.work_source). For a Linear work source that is
	// the Linear team mapped to the criterion's repo; for GitHub (or an unset
	// work source) it is identical to ACMMIssueTrackerGitHub.
	ACMMIssueTrackerWorkSource = "work_source"
)

// ValidACMMIssueTrackers is the accepted set for governor.acmm.issue_tracker.
// "" is accepted and means "use the default" (GitHub).
var ValidACMMIssueTrackers = map[string]bool{
	"":                         true,
	ACMMIssueTrackerGitHub:     true,
	ACMMIssueTrackerWorkSource: true,
}

// ValidateACMMIssueTracker reports whether v is an accepted issue_tracker value.
func ValidateACMMIssueTracker(v string) bool { return ValidACMMIssueTrackers[v] }

// ACMMConfig tunes the dashboard's ACMM evaluation surface.
type ACMMConfig struct {
	// IssueTracker selects where the dashboard's "Open Issue" / "Open All"
	// buttons on a failed ACMM criterion file the gap issue.
	//
	// Valid values: "" (= github) | github | work_source.
	// See ACMMIssueTrackerWorkSource for what work_source resolves to. A
	// request may override this per click via the `tracker` field of
	// POST /api/acmm/issue.
	IssueTracker string `yaml:"issue_tracker,omitempty" json:"issue_tracker,omitempty"`
}

// EffectiveIssueTracker resolves the configured tracker with its default:
// "" → github. It never returns an invalid value; Validate rejects those at
// load time and unknown strings degrade to the default here so a stale
// in-memory config can never route an issue somewhere unexpected.
func (c ACMMConfig) EffectiveIssueTracker() string {
	if strings.TrimSpace(c.IssueTracker) == ACMMIssueTrackerWorkSource {
		return ACMMIssueTrackerWorkSource
	}
	return ACMMIssueTrackerGitHub
}

// ResolveACMMIssueTracker picks the tracker for one issue-creation request:
// the request override when non-empty, else the configured default. An
// unknown override is an error (the API turns it into a 400) rather than a
// silent fallback — an operator who typed "linear" must learn that the
// accepted spelling is "work_source", not get a GitHub issue.
func (c ACMMConfig) ResolveACMMIssueTracker(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		return c.EffectiveIssueTracker(), nil
	}
	if !ValidateACMMIssueTracker(override) {
		return "", fmt.Errorf("invalid tracker %q (must be %s or %s)", override, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource)
	}
	return override, nil
}
