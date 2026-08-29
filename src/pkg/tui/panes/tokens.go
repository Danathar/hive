package panes

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TokenCounts is the numeric half of a tokens row: what was spent, and what it
// is estimated to have cost.
type TokenCounts struct {
	// Input and Output are token counts as the dashboard reports them.
	Input  int64
	Output int64

	// CostUSD is the ESTIMATED dollar cost, and CostKnown says whether it was
	// fetched at all.
	//
	// The two fields exist separately because $0.00 and "not known" are
	// different facts and a single float cannot tell them apart. That
	// distinction is load-bearing here rather than hypothetical: the endpoint
	// T8 (#5057) targets, GET /api/tokens, carries NO cost field —
	// tokens.AggregateSummary (src/pkg/tokens/collector.go:160) is token counts
	// only. Dollar cost is published by GET /api/cost, whose
	// `estimated.by_agent[]` rows carry name, input, output and usd together
	// (src/pkg/dashboard/cost.go:155, and the spec's /api/cost operation, which
	// shares the "Tokens" tag). Until a fetch supplies it, a row renders "—"
	// in the COST column rather than a fabricated $0.00.
	CostUSD   float64
	CostKnown bool
}

// total is the row's magnitude for ordering purposes.
func (c TokenCounts) total() int64 { return c.Input + c.Output }

// TokenRow is one agent's usage.
type TokenRow struct {
	// Agent is the agent's name as the dashboard keys usage by.
	Agent string
	TokenCounts
}

// TokensMsg delivers a completed token-usage read to the Tokens pane.
//
// It is the pane's own message type rather than a shared "data arrived"
// message because the app broadcasts non-key messages to EVERY pane (the T3
// routing contract); a shared type would make every pane inspect a payload
// addressed to another. The app sends this in T12/T13b; T9 only defines and
// renders it.
//
// WHY THIS CARRIES VIEW ROWS, NOT A CLIENT TYPE. T8 (the /api/tokens client)
// has not merged, so there is no client type to embed — but even once it has,
// this shape is the right one: the sketch's four columns do not come from one
// endpoint. Agent/in/out are on /api/tokens; the cost is on /api/cost. The
// pane's contract is "the rows to draw", and assembling them from however many
// endpoints that takes is the fetching task's job, which is explicitly out of
// scope here.
type TokensMsg struct {
	// Agents is one row per agent, in any order — the pane sorts.
	Agents []TokenRow

	// Total is the fleet total AS THE DASHBOARD REPORTS IT, not the sum of
	// Agents.
	//
	// It is carried rather than computed on purpose. AggregateSummary's
	// TotalInput/TotalOutput count every scanned session, including sessions
	// the collector could not attribute to a configured agent; re-summing the
	// per-agent rows would quietly produce a smaller number than the web
	// dashboard shows for the same hive, and an operator comparing the two
	// would have no way to tell which was wrong.
	Total TokenCounts
}

// Tokens is the token-usage pane: one row per agent (agent, in, out, cost), a
// separator, and a totals row, per the design sketch in
// src/docs/design/tui.md §3.
type Tokens struct {
	stub

	// rows is the most recent delivered usage, already sorted for display.
	rows []TokenRow

	// total is the delivered fleet total. See TokensMsg.Total.
	total TokenCounts

	// loaded records that data has arrived at least once. Without it an empty
	// rows slice is ambiguous — a hive whose agents have burned no tokens yet
	// and a TUI that has not fetched anything would render identically, and
	// "waiting for data" would be a lie in the first case.
	loaded bool
}

// NewTokens returns the Tokens pane in its pre-data state.
func NewTokens() Tokens { return Tokens{stub: stub{title: "TOKENS"}} }

// Update implements Pane. A TokensMsg replaces the pane's usage; every other
// message falls through to the stub behaviour (see stub.update for why a pane
// still returns itself).
func (p Tokens) Update(msg tea.Msg) (Pane, tea.Cmd) {
	if data, ok := msg.(TokensMsg); ok {
		p.rows = sortRows(data.Agents)
		p.total = data.Total
		p.loaded = true
		return p, nil
	}
	return p.update(msg, p)
}

// sortRows orders rows by total tokens descending, then by name ascending.
//
// The pane sorts rather than trusting its input because the dashboard's usage
// is keyed by MAP (AggregateSummary.ByAgentDetail), and Go randomizes map
// iteration: an unsorted pane would reshuffle its rows on every poll, turning
// a 5-second refresh into a flicker. Descending by spend also means the agent
// worth looking at is the one that survives when the pane is too short to show
// them all; the name tie-break keeps the order total, so equal-spend agents do
// not swap places between frames either.
//
// It copies rather than sorting in place: the caller's slice belongs to the
// message, and a pane is not entitled to reorder someone else's memory.
func sortRows(in []TokenRow) []TokenRow {
	rows := make([]TokenRow, len(in))
	copy(rows, in)
	sort.Slice(rows, func(i, j int) bool {
		if a, b := rows[i].total(), rows[j].total(); a != b {
			return a > b
		}
		return rows[i].Agent < rows[j].Agent
	})
	return rows
}

// Column widths for the three numeric columns. The agent column takes
// whatever is left, so a wider pane spends its extra space on names — the
// column that actually varies — instead of padding numbers that never need it.
const (
	tokensInWidth   = 9
	tokensOutWidth  = 9
	tokensCostWidth = 8

	// tokensNumericWidth is the three numeric columns plus the single space
	// separating each pair.
	tokensNumericWidth = tokensInWidth + tokensOutWidth + tokensCostWidth + 3

	// tokensMinNameWidth keeps the agent column from collapsing entirely in a
	// pane too narrow for the table. The row is allowed to overrun and be
	// clipped at that point; T24 owns the operator-facing minimum-size story.
	tokensMinNameWidth = 6

	// tokensChrome is the lines the table spends on something other than an
	// agent row: title, blank, column header, separator, totals.
	tokensChrome = 5
)

// View implements Pane.
func (p Tokens) View(width, height int) string {
	if width <= 0 || height <= 0 {
		// A degenerate box renders as nothing rather than corrupting the grid
		// with unpadded lines, matching every other pane.
		return ""
	}
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}

	nameWidth := max(tokensMinNameWidth, width-tokensNumericWidth)
	lines := []string{tokensRow(nameWidth, "AGENT", "IN", "OUT", "COST")}
	lines = append(lines, p.agentLines(nameWidth, height)...)
	lines = append(lines,
		strings.Repeat("─", min(width, nameWidth+tokensNumericWidth)),
		tokensRow(nameWidth, "total",
			formatMagnitude(p.total.Input),
			formatMagnitude(p.total.Output),
			formatCost(p.total)),
	)

	// No color or emphasis anywhere in this table, deliberately. lipgloss
	// resolves styles against the detected terminal profile, which is Ascii
	// under `go test`; a bolded header would render differently here than on
	// an operator's terminal and the golden file would pin whichever one CI
	// happened to have. Alignment carries the structure instead.
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(p.Title() + "\n\n" + strings.Join(lines, "\n"))
}

// agentLines renders as many agent rows as the pane is tall enough to show.
//
// When they do not all fit the LAST visible row is spent saying how many were
// dropped, rather than silently truncating: a pane showing three of nine
// agents with no indication reads as a complete fleet, and an operator would
// have no reason to suspect the total row disagrees with the rows above it.
func (p Tokens) agentLines(nameWidth, height int) []string {
	if len(p.rows) == 0 {
		return []string{tokensRow(nameWidth, "no usage recorded", "", "", "")}
	}

	visible := height - tokensChrome
	if visible <= 0 {
		// Too short for even one row: the totals still fit and are the more
		// useful of the two, so they are what survives.
		return nil
	}

	shown := p.rows
	var overflow string
	if len(shown) > visible {
		shown = shown[:visible-1]
		overflow = fmt.Sprintf("… +%d more", len(p.rows)-len(shown))
	}

	lines := make([]string, 0, visible)
	for _, r := range shown {
		lines = append(lines, tokensRow(nameWidth, r.Agent,
			formatMagnitude(r.Input), formatMagnitude(r.Output), formatCost(r.TokenCounts)))
	}
	if overflow != "" {
		lines = append(lines, tokensRow(nameWidth, overflow, "", "", ""))
	}
	return lines
}

// tokensRow lays out one line of the table: the agent column left-aligned, the
// three numeric columns right-aligned.
//
// Right-aligning the numbers is a deliberate departure from the §3 sketch,
// which left-aligns them. Magnitudes carry a unit suffix, so "1.2M" and
// "88.1k" are the same width while differing by three orders of magnitude;
// left-aligned they invite exactly the misreading the pane exists to prevent.
// The sketch is explicitly "the target ... not a golden file".
func tokensRow(nameWidth int, name, in, out, cost string) string {
	return fmt.Sprintf("%-*s %*s %*s %*s",
		nameWidth, truncate(name, nameWidth),
		tokensInWidth, in,
		tokensOutWidth, out,
		tokensCostWidth, cost)
}

// truncate clips a cell to width, spending the last cell on an ellipsis so a
// cut name is visibly cut. Widths here are ASCII-dominated agent names; the
// rune count is what matters, not the byte count.
func truncate(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// formatMagnitude renders a token count the way the sketch does: 1.2M, 88.1k,
// and plain digits below a thousand.
func formatMagnitude(n int64) string {
	sign, v := "", float64(n)
	if n < 0 {
		sign, v = "-", -v
	}
	const step = 1000.0
	if v < step {
		return sign + strconv.FormatInt(int64(v), 10)
	}
	units := []string{"k", "M", "B"}
	for i := 0; ; i++ {
		v /= step
		// Round BEFORE deciding whether to promote: 999,950 tokens rounds to
		// 1000.0k, which is four digits of the wrong unit. It reads 1.0M.
		rounded := math.Round(v*10) / 10
		if rounded < step || i == len(units)-1 {
			return sign + strconv.FormatFloat(rounded, 'f', 1, 64) + units[i]
		}
	}
}

// formatCost renders the estimated dollar cost, or an em dash when no cost was
// fetched. See TokenCounts.CostKnown for why the two are distinguishable.
func formatCost(c TokenCounts) string {
	if !c.CostKnown {
		return "—"
	}
	return "$" + strconv.FormatFloat(c.CostUSD, 'f', 2, 64)
}
