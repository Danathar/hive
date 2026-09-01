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
// T3 (#5004) shipped the interface and four stubs that each render their title
// and a "waiting for data" placeholder. T12 (#5061) adds the poll loop's
// delivery types — a pane's data arrives as its own message type (AgentsMsg,
// and one per pane as the remaining client tasks land) and the pane keeps it.
// Real content per pane arrives in T5/T7/T9/T11.
package panes

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// placeholder is what a pane shows under its title before its first
// successful poll. Exported to the tests via Title()+View() only.
//
// It means "nothing has arrived yet", NOT "there is nothing" — a pane that
// has polled successfully and received an empty list must say so differently,
// or an operator cannot tell a broken fetch from an empty fleet.
const placeholder = "waiting for data"

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

// contentView renders a pane's title and body filled to exactly width×height
// so the grid's cells stay rectangular regardless of content.
//
// Every pane renders through here rather than styling its own box: the grid's
// join math depends on each cell being exactly the size it was given, and one
// pane getting that wrong skews the whole frame.
func contentView(title, body string, width, height int) string {
	if width <= 0 || height <= 0 {
		// A degenerate box renders as nothing rather than corrupting the
		// grid with unpadded lines. T24 owns the operator-facing
		// minimum-size story; this only keeps the join math safe.
		return ""
	}
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(title + "\n\n" + body)
}

// stubView is contentView with the "no data yet" body: the shared layout for
// a pane that is waiting for the task which fills it.
//
// Kept in pane.go rather than repeated four times: the per-pane files stay
// "no logic" as T3 specifies, and when the first real pane replaces its stub
// the shared helper keeps working for the ones still waiting.
func stubView(title string, width, height int) string {
	return contentView(title, placeholder, width, height)
}

// stub is the common implementation behind the placeholder panes.
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
