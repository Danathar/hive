package panes

import tea "github.com/charmbracelet/bubbletea"

// Governor is the governor-status pane — mode, queue depth and next eval once T7
// lands. Until then it is a stub that names itself and waits.
type Governor struct{ stub }

// NewGovernor returns the Governor pane stub.
func NewGovernor() Governor { return Governor{stub{title: "GOVERNOR"}} }

// Update implements Pane. See stub.update for why a stub still returns itself.
func (p Governor) Update(msg tea.Msg) (Pane, tea.Cmd) { return p.update(msg, p) }
