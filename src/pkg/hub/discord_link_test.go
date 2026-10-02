package hub

import (
	"io/fs"
	"strings"
	"testing"
)

func TestHubStaticPagesHaveDiscordLinks(t *testing.T) {
	pages := []string{
		"static/index.html",
		"static/learn.html",
		"static/reading.html",
		"static/get-started.html",
		"static/api-docs.html",
		"static/my-hives.html",
	}
	for _, page := range pages {
		b, err := fs.ReadFile(staticFS, page)
		if err != nil {
			t.Fatalf("reading embedded %s: %v", page, err)
		}
		html := string(b)
		for _, want := range []string{`href="https://hivecommons.dev/discord"`, `target="_blank"`, `rel="noopener"`, `Join our Discord`} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing Discord link snippet %q", page, want)
			}
		}
	}
}
