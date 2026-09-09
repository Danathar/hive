package github

import "strings"

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
