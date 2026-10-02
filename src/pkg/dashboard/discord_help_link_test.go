package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardHelpGroupHasDiscordLink(t *testing.T) {
	html := indexHTML(t)
	want := `<a class="oc-nav-item" href="https://hivecommons.dev/discord" target="_blank" rel="noopener"><span class="oc-nav-emoji">💬</span><span class="oc-nav-text">Join our Discord</span></a>`
	if !strings.Contains(html, want) {
		t.Fatalf("dashboard Help group missing Discord link with safe external-link attributes")
	}
	guide := strings.Index(html, `<span class="oc-nav-text">Getting Started Guide</span>`)
	discord := strings.Index(html, want)
	issue := strings.Index(html, `<span class="oc-nav-text">Report an Issue</span>`)
	if guide == -1 || discord == -1 || issue == -1 || !(guide < discord && discord < issue) {
		t.Fatalf("Discord help link should appear after Getting Started Guide and before Report an Issue")
	}
}
