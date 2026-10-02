package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestDashboardSectionsUseSharedCardChrome(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function sectionCardHeader(opts)",
		"function sectionCardShell(opts, bodyHtml)",
		"function ensureSectionCard(sectionId)",
		"function refreshSectionCardShell(sectionId)",
		"function initializeDashboardSectionCards()",
		`data-section-card-header="1"`,
		"dash-card-summary",
		"refreshDashboardSectionSummaries(data)",
		"sectionHeaderKey",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shared dashboard card chrome is missing %q", want)
		}
	}

	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for _, sectionID := range dashboardLayoutTemplateIDs(t, html) {
		entry := dashboardSectionConfigEntry(t, config, sectionID)
		if !strings.Contains(entry, "summary:") {
			t.Fatalf("section %q card config lacks collapsed summary", sectionID)
		}
		if title := dashboardSectionConfigTitle(t, entry); !startsWithEmoji(title) {
			t.Fatalf("section %q title %q does not start with an emoji", sectionID, title)
		}
	}

	for _, fn := range []string{"function renderGovernor(gov, cadenceMatrix, data)", "function renderTokens(tokens)", "function renderCost(cost)"} {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "sectionCardShell({") {
			t.Fatalf("%s does not render through sectionCardShell", fn)
		}
		if !strings.Contains(body, "applySectionCollapse(") && !strings.Contains(body, "applyCostCollapse()") {
			t.Fatalf("%s does not re-apply persisted section collapse state", fn)
		}
	}
}

func TestDashboardSectionEmojisMatchSidebar(t *testing.T) {
	html := indexHTML(t)
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for sectionID, emoji := range sidebarSectionEmojis(html) {
		entry := dashboardSectionConfigEntry(t, config, sectionID)
		title := dashboardSectionConfigTitle(t, entry)
		if !strings.HasPrefix(title, emoji) {
			t.Fatalf("section %q title %q does not match sidebar emoji %q", sectionID, title, emoji)
		}
	}
}

func TestDashboardSectionCardActionsStopPropagationAndShareClass(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-action="toggleOverviewChartSettings" data-stop="1"`,
		`id="repos-rescan-btn" data-action="reposForceRescan" data-stop="1"`,
		`id="repos-reset-layout-btn" data-action="resetRepoCardWidths" data-stop="1"`,
		`data-action="openACMMDialog" data-stop="1"`,
		`id="acmm-refresh-btn" data-action="acmmForceRefresh" data-stop="1"`,
		`data-action="openNousConfig" data-stop="1"`,
		`id="agents-compact-all-btn" data-action="toggleAllAgentsCompact" data-stop="1"`,
		`data-action="openConfigDialog" data-arg0="governor" data-stop="1"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("header action is missing data-stop propagation guard: %s", want)
		}
	}

	normalizeBody := jsFunctionBody(t, html, "function normalizeSectionCardChrome(sectionId)")
	for _, want := range []string{
		".dash-card-actions > *",
		"classList.add('dash-card-action')",
		".dash-card-badges > *",
		"classList.add('dash-card-badge')",
	} {
		if !strings.Contains(normalizeBody, want) {
			t.Fatalf("shared header action/badge normalization is missing %q", want)
		}
	}
	ensureBody := jsFunctionBody(t, html, "function ensureSectionCard(sectionId)")
	if !strings.Contains(ensureBody, `el.setAttribute('data-stop', '1')`) {
		t.Fatal("runtime-migrated header links/buttons are not forced to stop propagation")
	}
}

func TestDashboardSectionRenderersRefreshSharedShell(t *testing.T) {
	html := indexHTML(t)
	cases := map[string]string{
		"function renderRepos(repos)":               "repos-section",
		"function renderBeads(beads)":               "beads-section",
		"function renderAgents(agents)":             "agents-section",
		"function acmmRenderCard()":                 "acmm-eval-section",
		"function renderApprovals(dto)":             "approvals-section",
		"function renderContributors(contributors)": "contributors-section",
		"function renderAuditTable(entries)":        "audit-section",
		"function renderReviewQueue(data)":          "review-queue-section",
		"function renderFAQ()":                      "faq-section",
		"function renderPlatform(plat)":             "platform-section",
		"function renderInception()":                "inception-section",
		"function renderKnowledge()":                "knowledge-section",
		"function renderDebugSection()":             "debug-section",
		"function renderLogs()":                     "logs-section",
	}
	for fn, sectionID := range cases {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "refreshSectionCardShell('"+sectionID+"')") {
			t.Fatalf("%s does not refresh shared shell for %s", fn, sectionID)
		}
	}
}

func TestDashboardSectionChromeHasNoSectionSpecificOverrides(t *testing.T) {
	html := indexHTML(t)
	for _, forbidden := range []string{
		".contributors-grip", ".agents-grip", ".contributors-title", ".agents-title",
		".contributors-chevron", ".agents-chevron",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("section-specific dashboard card chrome override remains: %s", forbidden)
		}
	}
	for _, want := range []string{
		".dash-card-header .dashboard-grip",
		".dash-card-header .section-chevron",
		".dash-card-title",
		".dash-card-action",
		".dash-card-badge",
		".dash-card.collapsed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shared dashboard card CSS is missing %q", want)
		}
	}
}

func TestCostPanelUsesGenericSectionPersistence(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"if (sectionId === 'cost-panel' && localStorage.getItem('hive-cost-collapsed') === '1') return true;",
		"if (sectionId === 'cost-panel') {",
		"function isCostCollapsed() {\n      return isSectionCollapsed('cost-panel');",
		"function setCostCollapsed(collapsed) {\n      setSectionCollapsed('cost-panel', collapsed);",
		"action: 'toggleCostPanel'",
		"summaryClass: 'cost-collapsed-total'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("cost panel did not converge on generic section persistence/component: missing %q", want)
		}
	}
}

func dashboardLayoutTemplateIDs(t *testing.T, html string) []string {
	t.Helper()
	re := regexp.MustCompile(`var DASHBOARD_LAYOUT_TEMPLATE=\{main:\[([^\]]+)\]\}`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("dashboard layout template not found")
	}
	idRe := regexp.MustCompile(`'([^']+)'`)
	matches := idRe.FindAllStringSubmatch(m[1], -1)
	if len(matches) == 0 {
		t.Fatal("dashboard layout template has no section ids")
	}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, match[1])
	}
	return ids
}

func jsConstObject(t *testing.T, html, decl string) string {
	t.Helper()
	i := strings.Index(html, decl)
	if i < 0 {
		t.Fatalf("index.html has no %q", decl)
	}
	rest := html[i:]
	loc := regexp.MustCompile(`(?m)^    \}\);\n`).FindStringIndex(rest)
	if loc == nil {
		t.Fatalf("could not find the end of %q", decl)
	}
	return rest[:loc[1]]
}

func dashboardSectionConfigEntry(t *testing.T, config, sectionID string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta("'"+sectionID+"'") + `:\s*\{([^}]+)\}`)
	m := re.FindStringSubmatch(config)
	if m == nil {
		t.Fatalf("section %q is not registered for shared dashboard card chrome", sectionID)
	}
	return m[1]
}

func dashboardSectionConfigTitle(t *testing.T, entry string) string {
	t.Helper()
	re := regexp.MustCompile(`title:\s*'([^']+)'`)
	m := re.FindStringSubmatch(entry)
	if m == nil {
		t.Fatalf("section config entry has no title: %s", entry)
	}
	return m[1]
}

func sidebarSectionEmojis(html string) map[string]string {
	re := regexp.MustCompile(`data-section="([^"]+)"[^>]*>\s*<span class="oc-nav-emoji">([^<]+)</span>`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func startsWithEmoji(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8DecodeRuneInString(s)
	return r > 0x2600
}

func utf8DecodeRuneInString(s string) (rune, int) {
	for i, r := range s {
		return r, i
	}
	return 0, 0
}
