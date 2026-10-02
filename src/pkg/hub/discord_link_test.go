package hub

import (
	"strings"
	"testing"
)

func TestHubStaticIndexHasDiscordLink(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{`href="https://hivecommons.dev/discord"`, `target="_blank"`, `rel="noopener"`, `Join our Discord`} {
		if !strings.Contains(html, want) {
			t.Fatalf("hub static index missing Discord link snippet %q", want)
		}
	}
}
