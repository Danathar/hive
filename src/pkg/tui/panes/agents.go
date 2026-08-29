package panes

import tea "github.com/charmbracelet/bubbletea"

// Agents is the fleet pane — one row per agent with state and backend once T5
// lands. Until then it is a stub that names itself and waits.
type Agents struct{ stub }

// NewAgents returns the Agents pane stub.
func NewAgents() Agents { return Agents{stub{title: "AGENTS"}} }

// Update implements Pane. See stub.update for why a stub still returns itself.
func (p Agents) Update(msg tea.Msg) (Pane, tea.Cmd) { return p.update(msg, p) }
