package dashboard

import (
	"os"
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

func TestDashboardFAQWelcomeAndFeedbackMentionDiscord(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="faq-panel"`,
		`Join our Discord</a>; the Hive Commons community can help`,
		`New to Hive? Start here`,
		`href="https://hivecommons.dev/discord" target="_blank" rel="noopener"`,
		`nps-card-discord`,
		`Need help? Join our Discord`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard static index missing Discord snippet %q", want)
		}
	}
}

func TestContributeLandingMentionsDiscord(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`Questions? <a class="discord-link" href="https://hivecommons.dev/discord"`,
		`we'll help you get to your first PR`,
		`placeholder="Chat with other contributors | https://hivecommons.dev/discord`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("contribute landing missing Discord snippet %q", want)
		}
	}
}
