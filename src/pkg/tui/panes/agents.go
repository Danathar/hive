package panes

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/client"
)

// AgentsMsg delivers a completed GET /api/agents poll to the Agents pane.
//
// It is the app's poll loop (T12, #5061) that fetches and the pane that keeps
// the result; this type is the contract between them. Each pane owns its own
// message type rather than sharing one "data arrived" message, because the
// app broadcasts non-key messages to EVERY pane (the T3 routing contract) —
// a shared type would make every pane inspect a payload addressed to another.
//
// Only SUCCESSFUL polls become an AgentsMsg. A failed fetch never reaches a
// pane at all (see fetchErrMsg in pkg/tui/app.go), which is what makes "a
// failed poll keeps the previous data" true by construction rather than by a
// rule each pane has to remember to follow.
type AgentsMsg struct {
	// Agents is the fleet as of that poll. An empty (or nil) slice is a
	// legitimate value meaning "this hive has no agents configured", NOT a
	// failure — see the Agents pane's `loaded` flag for why the difference
	// has to be representable.
	Agents []client.Agent
}

// Agents is the fleet pane — one row per agent with state and backend once T5
// lands.
//
// T12 gives it the data and the honest two-state summary that goes with
// having data; the row-per-agent table, the status glyphs and the j/k
// selection cursor are T5's, and they replace summaryLine without touching
// the delivery plumbing here.
type Agents struct {
	stub

	// agents is the most recent SUCCESSFUL poll. A failed poll leaves it
	// alone, so the pane keeps showing the last fleet it actually saw
	// instead of blanking on a transient error.
	agents []client.Agent

	// loaded records that at least one poll has succeeded. Without it an
	// empty `agents` is ambiguous — a hive with no agents configured and a
	// TUI that has not yet fetched anything would render identically, and
	// "waiting for data" would be a lie in the first case.
	loaded bool
}

// NewAgents returns the Agents pane in its pre-poll state.
func NewAgents() Agents { return Agents{stub: stub{title: "AGENTS"}} }

// Update implements Pane. An AgentsMsg replaces the pane's fleet; every other
// message falls through to the stub behaviour (see stub.update for why a pane
// still returns itself).
func (p Agents) Update(msg tea.Msg) (Pane, tea.Cmd) {
	if data, ok := msg.(AgentsMsg); ok {
		p.agents = data.Agents
		p.loaded = true
		return p, nil
	}
	return p.update(msg, p)
}

// View implements Pane.
func (p Agents) View(width, height int) string {
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}
	return contentView(p.Title(), p.summaryLine(), width, height)
}

// summaryLine is deliberately the smallest honest thing the pane can say once
// it holds real data: how many agents the poll returned.
//
// It exists because the alternative is worse, not because it is the intended
// UI. Leaving the placeholder up after a successful poll would state that no
// data has arrived when some has — so the pane has to change, and this is the
// change that commits to none of T5's layout decisions (columns, glyphs,
// truncation, selection). T5 replaces this one line with the table.
func (p Agents) summaryLine() string {
	switch n := len(p.agents); n {
	case 0:
		return "no agents configured"
	case 1:
		return "1 agent"
	default:
		return fmt.Sprintf("%d agents", n)
	}
}
