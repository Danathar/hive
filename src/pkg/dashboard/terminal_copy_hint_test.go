package dashboard

import (
	"strings"
	"testing"
)

// TestTerminalCopyHints pins the usable fallback for issue #5188. Released
// ttyd does not consume OSC52 clipboard writes, and tmux mouse mode intercepts
// ordinary selection, so both dashboard agent layouts must explain
// Shift-selection next to their terminal controls. Login-blocked agents must
// keep the stronger URL warning even after the ordinary hint has been dismissed.
func TestTerminalCopyHints(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	for _, want := range []string{
		"const TERMINAL_COPY_HINT_LS_KEY = 'hive-terminal-copy-hint-dismissed';",
		"try { return localStorage.getItem(TERMINAL_COPY_HINT_LS_KEY) === '1'; }",
		"if (sessionUnavailable) return '';",
		"if (!needsLogin && terminalCopyHintDismissed()) return '';",
		"You’ll be copying a URL: hold <strong>Shift</strong> while selecting",
		"Wrapped URLs may copy with line breaks — check before submitting.",
		"Terminal copy: <strong>⇧-drag</strong> to select",
		"const dismiss = needsLogin ? '' : '<button type=\"button\"",
		"try { localStorage.setItem(TERMINAL_COPY_HINT_LS_KEY, '1'); } catch {}",
		".terminal-copy-hint:not(.needs-login)",
		"data-action=\"dismissTerminalCopyHint\"",
		"${terminalCopyHintHtml(needsLoginDown, agentSessionUnavailable(a))}",
		"${terminalCopyHintHtml(needsLogin, agentSessionUnavailable(a))}",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("static dashboard terminal copy guidance is missing %q", want)
		}
	}

	if got := strings.Count(html, "${terminalCopyHintHtml("); got != 2 {
		t.Errorf("terminal copy hint rendered at %d call sites, want 2 (operator-console detail and legacy agent card)", got)
	}
	if strings.Contains(html, `onclick="dismissTerminalCopyHint`) {
		t.Error("terminal copy hint reintroduced an inline event handler; CSP requires data-action delegation")
	}
}
