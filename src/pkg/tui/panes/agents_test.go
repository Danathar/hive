package panes

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/client"
)

func agent(name string) client.Agent {
	return client.Agent{Name: name, ID: "agt_" + name, Backend: "claude", Model: "claude-opus-4-5", Enabled: true}
}

// TestAgentsStopsWaitingOnceDataArrives pins the honesty rule the pane's
// `loaded` flag exists for: "waiting for data" must stop being shown the
// moment data has in fact arrived, including when that data is an empty fleet.
func TestAgentsStopsWaitingOnceDataArrives(t *testing.T) {
	if view := NewAgents().View(40, 10); !strings.Contains(view, placeholder) {
		t.Fatalf("pre-poll view missing %q:\n%s", placeholder, view)
	}

	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	view := loaded.View(40, 10)
	if strings.Contains(view, placeholder) {
		t.Fatalf("view still shows %q after a successful poll:\n%s", placeholder, view)
	}
	if !strings.Contains(view, "2 agents") {
		t.Fatalf("view does not report the fleet size:\n%s", view)
	}
}

// TestAgentsEmptyFleetIsNotWaiting is the case the flag exists to separate: a
// hive with no agents configured has polled successfully and must say so,
// rather than looking identical to a TUI that has fetched nothing.
func TestAgentsEmptyFleetIsNotWaiting(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{})
	view := loaded.View(40, 10)
	if strings.Contains(view, placeholder) {
		t.Fatalf("an empty fleet renders as %q, which claims nothing was fetched:\n%s", placeholder, view)
	}
	if !strings.Contains(view, "no agents configured") {
		t.Fatalf("an empty fleet does not say so:\n%s", view)
	}
}

// TestAgentsSingularWording: "1 agents" is the kind of detail that survives
// forever once shipped.
func TestAgentsSingularWording(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	view := loaded.View(40, 10)
	if !strings.Contains(view, "1 agent") || strings.Contains(view, "1 agents") {
		t.Fatalf("single-agent fleet reads wrong:\n%s", view)
	}
}

// TestAgentsKeepsDataAcrossForeignMessages pins the pane's half of "a failed
// poll keeps the previous data". The app swallows errors, so what a pane
// actually sees between two successful polls is other panes' messages — and
// none of them may clear its fleet.
func TestAgentsKeepsDataAcrossForeignMessages(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	before := loaded.View(40, 10)

	type otherPaneMsg struct{ Data string }
	after, cmd := loaded.Update(otherPaneMsg{Data: "not for you"})
	if cmd != nil {
		t.Error("a foreign message produced a command")
	}
	if got := after.View(40, 10); got != before {
		t.Errorf("a foreign message changed the pane:\nbefore:\n%s\nafter:\n%s", before, got)
	}

	keyed, _ := after.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if got := keyed.View(40, 10); got != before {
		t.Error("a key changed a pane with no bindings")
	}
}

// TestAgentsViewFillsItsBoxExactly is the grid's structural requirement: a
// pane renders exactly the size it was given whatever its content, or the 2×2
// join skews.
func TestAgentsViewFillsItsBoxExactly(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	for _, dims := range [][2]int{{40, 10}, {20, 5}, {80, 24}} {
		w, h := dims[0], dims[1]
		view := loaded.View(w, h)
		if lines := strings.Count(view, "\n") + 1; lines != h {
			t.Errorf("View(%d,%d) rendered %d lines, want %d", w, h, lines, h)
		}
		if vw := visibleWidth(view); vw != w {
			t.Errorf("View(%d,%d) widest line is %d cells, want %d", w, h, vw, w)
		}
	}
	for _, dims := range [][2]int{{0, 5}, {5, 0}, {-1, -1}} {
		if got := loaded.View(dims[0], dims[1]); got != "" {
			t.Errorf("View(%d,%d) = %q, want empty", dims[0], dims[1], got)
		}
	}
}
