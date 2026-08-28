package panes

import tea "github.com/charmbracelet/bubbletea"

// Events is the event-feed pane — the scrollback of kicks, PRs and state changes once T11
// lands. Until then it is a stub that names itself and waits.
type Events struct{ stub }

// NewEvents returns the Events pane stub.
func NewEvents() Events { return Events{stub{title: "EVENTS"}} }

// Update implements Pane. See stub.update for why a stub still returns itself.
func (p Events) Update(msg tea.Msg) (Pane, tea.Cmd) { return p.update(msg, p) }
