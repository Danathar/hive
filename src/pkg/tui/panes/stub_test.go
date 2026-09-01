package panes

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// visibleWidth reports the rendered width of the widest line, used to pin
// that stubs fill their box exactly.
func visibleWidth(rendered string) int {
	widest := 0
	for _, line := range strings.Split(rendered, "\n") {
		if w := lipgloss.Width(line); w > widest {
			widest = w
		}
	}
	return widest
}

// TestStubPanesRenderTitleAndPlaceholder pins the T3 contract for all four
// stubs at once: each names itself and shows the shared placeholder, inside
// a box of exactly the requested size — the property the grid's join math
// relies on to stay rectangular.
func TestStubPanesRenderTitleAndPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		pane  Pane
		title string
	}{
		{NewAgents(), "AGENTS"},
		{NewGovernor(), "GOVERNOR"},
		{NewTokens(), "TOKENS"},
		{NewEvents(), "EVENTS"},
	} {
		if got := tc.pane.Title(); got != tc.title {
			t.Fatalf("Title() = %q, want %q", got, tc.title)
		}
		const w, h = 40, 10
		view := tc.pane.View(w, h)
		for _, want := range []string{tc.title, placeholder} {
			if !strings.Contains(view, want) {
				t.Fatalf("%s View() missing %q:\n%s", tc.title, want, view)
			}
		}
		if lines := strings.Count(view, "\n") + 1; lines != h {
			t.Fatalf("%s View() renders %d lines, want exactly %d", tc.title, lines, h)
		}
		if vw := visibleWidth(view); vw != w {
			t.Fatalf("%s View() widest line is %d cells, want exactly %d", tc.title, vw, w)
		}
	}
}

// TestStubPaneLifecycleIsInert pins that a stub issues no command from Init
// and ignores messages in Update while still returning ITSELF — the app
// stores Update's result back into its pane table, so a stub that returned a
// zero value would silently replace the pane on the first message.
func TestStubPaneLifecycleIsInert(t *testing.T) {
	for _, p := range []Pane{NewAgents(), NewGovernor(), NewTokens(), NewEvents()} {
		if cmd := p.Init(); cmd != nil {
			t.Fatalf("%s Init() returned a command, want nil", p.Title())
		}
		next, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		if cmd != nil {
			t.Fatalf("%s Update() returned a command, want nil", p.Title())
		}
		if next.Title() != p.Title() {
			t.Fatalf("stub Update() returned a different pane: %q, want %q", next.Title(), p.Title())
		}
	}
}

// TestStubViewDegeneratesSafely pins the zero/negative-size guard: a
// degenerate box must render as empty rather than panicking or emitting
// unpadded lines that would corrupt the grid join. T24 owns the operator-
// facing minimum-size message.
func TestStubViewDegeneratesSafely(t *testing.T) {
	for _, dims := range [][2]int{{0, 5}, {5, 0}, {-1, -1}} {
		if got := NewAgents().View(dims[0], dims[1]); got != "" {
			t.Fatalf("View(%d,%d) = %q, want empty", dims[0], dims[1], got)
		}
	}
}
