package panes

import tea "github.com/charmbracelet/bubbletea"

// Tokens is the token-usage pane — per-agent input/output/cost once T9
// lands. Until then it is a stub that names itself and waits.
type Tokens struct{ stub }

// NewTokens returns the Tokens pane stub.
func NewTokens() Tokens { return Tokens{stub{title: "TOKENS"}} }

// Update implements Pane. See stub.update for why a stub still returns itself.
func (p Tokens) Update(msg tea.Msg) (Pane, tea.Cmd) { return p.update(msg, p) }
