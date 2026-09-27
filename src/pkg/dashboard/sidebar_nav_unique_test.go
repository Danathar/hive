package dashboard

import (
	"regexp"
	"testing"
)

// The v5 dashboardSortSidebar() relocates every nav item whose data-section
// is in DASHBOARD_LAYOUT_TEMPLATE into the Dashboard group at runtime, so a
// section must be declared only once in the static sidebar or it renders twice.
func TestSidebarNavSectionsDeclaredOnce(t *testing.T) {
	html := indexHTML(t)
	re := regexp.MustCompile(`class="oc-nav-item[^"]*" data-section="([^"]+)"`)
	seen := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("sidebar nav item %q declared %d times", id, n)
		}
	}
}
