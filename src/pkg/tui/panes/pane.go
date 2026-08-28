// Package panes holds the four dashboard panes the TUI's 2×2 grid composes:
// Agents, Governor, Tokens and Events (kubestellar/hive#4907, layout in
// src/docs/design/tui.md §3).
//
// The split of responsibilities is deliberate and later tasks depend on it:
// a pane renders its own CONTENT into a box of the size it is given; the app
// (src/pkg/tui/app.go) owns everything around that box — the border, the
// focus highlight, and the grid geometry. Keeping chrome out of the panes
// means the focus style can change in exactly one place, and a pane's golden
// tests never break because the frame around it moved.
//
// T3 (#5004) ships the interface and four stubs that each render their title
// and a "waiting for data" placeholder. Real content arrives per pane in
// T5/T7/T9/T11, driven by the client calls of T4/T6/T8/T10.
package panes

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Pane is one cell of the TUI grid. It mirrors tea.Model deliberately — Init
// for the pane's first command, Update for messages — with two differences:
//
//   - Update returns Pane, not tea.Model, so the app can store the result
//     back into its pane table without a type assertion on every message.
//   - View takes the width and height the app's grid computed for this cell's
//     interior. A pane never knows the terminal size, only its own box; the
//     app resizes every pane by simply passing different numbers.
type Pane interface {
	Init() tea.Cmd
	Update(tea.Msg) (Pane, tea.Cmd)
	View(width, height int) string
	Title() string
}

// placeholder is what every stub pane shows under its title until the task
// that wires its data lands. Exported to the tests via Title()+View() only.
const placeholder = "waiting for data"

// stubView renders the shared stub layout: the pane's title on the first
// line, the placeholder beneath it, filled to exactly width×height so the
// grid's cells stay rectangular regardless of content.
//
// Kept in pane.go rather than repeated four times: the per-pane files stay
// "no logic" as T3 specifies, and when the first real pane replaces its stub
// the shared helper keeps working for the ones still waiting.
func stubView(title string, width, height int) string {
	if width <= 0 || height <= 0 {
		// A degenerate box renders as nothing rather than corrupting the
		// grid with unpadded lines. T24 owns the operator-facing
		// minimum-size story; this only keeps the join math safe.
		return ""
	}
	content := title + "\n\n" + placeholder
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(content)
}

// stub is the common implementation behind the four T3 placeholder panes.
// Each named pane type embeds one so that agents.go, governor.go, tokens.go
// and events.go each declare a distinct type — the grid, focus handling and
// later per-pane work key off those types — while the placeholder behaviour
// lives here once.
type stub struct {
	title string
}

func (s stub) Init() tea.Cmd { return nil }

// Update ignores every message: a stub has no state to advance. It still
// returns itself so the app's "store the updated pane back" plumbing is
// exercised from day one — the seam T5/T7/T9/T11 slot into.
func (s stub) update(msg tea.Msg, self Pane) (Pane, tea.Cmd) {
	_ = msg
	return self, nil
}

func (s stub) Title() string { return s.title }

func (s stub) View(width, height int) string {
	return stubView(s.title, width, height)
}
