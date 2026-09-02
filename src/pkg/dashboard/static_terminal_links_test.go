package dashboard

// Moved from static_index_test.go when the IndexDocument unit tests followed
// the type into pkg/dashboard/webstatic (#5565). This test pins the CONTENT of
// the embedded SPA document, which stays owned by pkg/dashboard (static/), so
// it stays here with it.

import (
	"os"
	"strings"
	"testing"
)

func TestStaticTerminalLinksRenewAssertionBeforeOpening(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const TERMINAL_ASSERTION_RENEW_PATH = '/api/terminal/assertion/renew';",
		"async function renewTerminalAssertion()",
		"credentials: 'same-origin'",
		"case 'openTerminal': e.preventDefault(); openTerminal(agent, el.href); break;",
		"data-action=\"openTerminal\"",
		"openTerminal(name);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard terminal renewal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "window.open(terminalUrl(name), '_blank', 'noopener');") {
		t.Fatal("welcome terminal action still opens /terminal directly without renewing the assertion")
	}
}
