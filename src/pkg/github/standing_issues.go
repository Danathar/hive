package github

import "strings"

// Standing meta issues — control panels and reports that are never completable
// work — must not enter the actionable set. Two kinds exist in practice:
//
//   - The hive's OWN advisory report (advisory.go): a digest the hive files
//     and updates itself. Offering it back to agents as work is circular, and
//     since it never closes it inflates every repo's actionable count by one,
//     forever.
//   - Bot dependency dashboards (Renovate's "Dependency Dashboard",
//     Dependabot's equivalent): standing, machine-maintained control panels.
//     Worse than noise: an agent that "works" one can tick its checkboxes,
//     which INSTRUCTS the bot to act (open PRs, unblock updates) — an agent
//     operating maintainer controls.
//
// The hub's contribute queue already encodes exactly this judgment for HUMAN
// contributors — its default deny lists match `*dependency dashboard*` /
// `*renovate dashboard*` titles and the renovate/dependabot/mergeraptor bot
// authors (config.Hub.ContributeDenyTitles/DenyAuthors) — but the agent path
// had no equivalent, so the same issue hive refuses to offer a person was
// counted "Actionable" on the dashboard and fed into every kick prompt. This
// filter closes that gap at the same choice point as the hold/exempt checks.
//
// This is a STRUCTURAL skip (like `issue.IsPullRequest()`), not a third
// operator-facing exclusion mechanism: issue_filter.go's one-exclusion-story
// (governor.labels.exempt is THE configurable exclude) is preserved. The
// bot-dashboard arm is deliberately narrow — a known bot author AND a
// dashboard title, mirroring the contribute-queue defaults — so a human's
// issue that merely mentions dependencies can never be swallowed.

// Standing meta issues are reports or bot-maintained control panels, not
// completable work. Keep the bot and title lists aligned with the contribute
// queue's default deny lists. Requiring both a known bot author and a dashboard
// title prevents human-authored issues with similar wording from being hidden.
var botControlPanelAuthors = []string{"renovate[bot]", "dependabot[bot]", "mergeraptor[bot]"}

var botControlPanelTitleFragments = []string{"dependency dashboard", "renovate dashboard"}

type standingMetaIssueKind uint8

const (
	standingMetaNone standingMetaIssueKind = iota
	standingMetaHiveAdvisory
	standingMetaDependencyDashboard
)

func standingMetaIssueKindFor(title, author string, labels []string) standingMetaIssueKind {
	if title == advisoryTitle {
		return standingMetaHiveAdvisory
	}
	for _, label := range labels {
		if strings.EqualFold(label, advisoryLabelName) {
			return standingMetaHiveAdvisory
		}
	}
	for _, bot := range botControlPanelAuthors {
		if !strings.EqualFold(author, bot) {
			continue
		}
		lowerTitle := strings.ToLower(title)
		for _, fragment := range botControlPanelTitleFragments {
			if strings.Contains(lowerTitle, fragment) {
				return standingMetaDependencyDashboard
			}
		}
	}
	return standingMetaNone
}

// standingMetaIssueReason preserves the diagnostic predicate introduced by
// #6158 while the typed classifier lets the repository breakdown distinguish
// advisory reports from dependency dashboards without reimplementing policy.
func standingMetaIssueReason(title, author string, labels []string) string {
	switch standingMetaIssueKindFor(title, author, labels) {
	case standingMetaHiveAdvisory:
		return "hive's own advisory report"
	case standingMetaDependencyDashboard:
		return "bot dependency dashboard (control panel, not work)"
	default:
		return ""
	}
}
