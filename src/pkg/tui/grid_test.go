package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/panes"
)

// fakePane exercises the branches the T3 stubs cannot: a pane whose Init and
// Update RETURN commands, and whose Update returns a changed pane. The app's
// plumbing for those paths ships in T3 so the pane tasks slot in without app
// changes — which means it must be tested now, not when its first real user
// lands.
type fakePane struct {
	title   string
	updates int
}

func (f *fakePane) Init() tea.Cmd { return func() tea.Msg { return nil } }
func (f *fakePane) Update(tea.Msg) (panes.Pane, tea.Cmd) {
	f.updates++
	return f, func() tea.Msg { return nil }
}
func (f *fakePane) View(width, height int) string { return f.title }
func (f *fakePane) Title() string                 { return f.title }

func modelWithFakes() (model, []*fakePane) {
	m := newModel()
	fakes := make([]*fakePane, paneCount)
	for i := range fakes {
		fakes[i] = &fakePane{title: m.panes[i].Title()}
		m.panes[i] = fakes[i]
	}
	return m, fakes
}

// TestNewExposesTheRootModel pins the exported constructor the golden test in
// pkg/tui/panes enters through.
func TestNewExposesTheRootModel(t *testing.T) {
	if _, ok := New().(model); !ok {
		t.Fatalf("New() = %T, want the root model", New())
	}
}

// TestInitBatchesPaneCommands pins that a pane's Init command is collected
// rather than dropped — the seam T5/T7/T9/T11 use to issue their first fetch.
func TestInitBatchesPaneCommands(t *testing.T) {
	m, _ := modelWithFakes()
	if m.Init() == nil {
		t.Fatal("Init() dropped the panes' initial commands")
	}
	// The stub panes have no initial work, so the real model issues none.
	if newModel().Init() != nil {
		t.Fatal("stub model Init() returned a command, want nil")
	}
}

// TestUnboundKeyRoutesToFocusedPaneOnly pins the key-routing seam: a key that
// is not a global binding reaches exactly the focused pane, and its command
// is propagated.
func TestUnboundKeyRoutesToFocusedPaneOnly(t *testing.T) {
	m, fakes := modelWithFakes()
	m.focus = 2

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if cmd == nil {
		t.Fatal("the focused pane's Update command was dropped")
	}
	for i, f := range fakes {
		want := 0
		if i == 2 {
			want = 1
		}
		if f.updates != want {
			t.Fatalf("pane %d saw %d updates, want %d — keys must reach only the focused pane", i, f.updates, want)
		}
	}
}

// TestNonKeyMessagesBroadcastToAllPanes pins the other half of the routing
// contract: a poll result or SSE event is not addressed to whichever pane
// happens to be focused, so every pane must see it.
func TestNonKeyMessagesBroadcastToAllPanes(t *testing.T) {
	m, fakes := modelWithFakes()

	type dataMsg struct{}
	_, cmd := m.Update(dataMsg{})
	if cmd == nil {
		t.Fatal("the panes' Update commands were dropped")
	}
	for i, f := range fakes {
		if f.updates != 1 {
			t.Fatalf("pane %d saw %d updates, want 1 — non-key messages broadcast to every pane", i, f.updates)
		}
	}
}
