package panes

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// usd is a shorthand for a row whose cost was fetched.
func usd(v float64) TokenCounts { return TokenCounts{CostUSD: v, CostKnown: true} }

// row builds a TokenRow with counts and a known cost.
func row(agent string, in, out int64, cost float64) TokenRow {
	c := usd(cost)
	c.Input, c.Output = in, out
	return TokenRow{Agent: agent, TokenCounts: c}
}

// TestFormatMagnitude pins the sketch's number format, including the two cases
// a naive implementation gets wrong: the promotion boundary (999,950 tokens is
// 1.0M, not 1000.0k) and sub-thousand counts, which stay plain digits rather
// than becoming "0.9k".
func TestFormatMagnitude(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1.0k"},
		{88_100, "88.1k"},
		{410_300, "410.3k"},
		{999_949, "999.9k"},
		{999_950, "1.0M"}, // rounds to 1000.0k in the wrong unit
		{1_200_000, "1.2M"},
		{1_749_000, "1.7M"},
		{999_950_000, "1.0B"},
		{2_500_000_000, "2.5B"},
		// Beyond the biggest unit the number simply grows; it does not wrap to
		// a unit that does not exist.
		{9_400_000_000_000, "9400.0B"},
		// Counts are never negative in practice, but a sign must not be
		// swallowed into a wrong-looking magnitude if one ever arrives.
		{-1_200_000, "-1.2M"},
		{-42, "-42"},
	} {
		if got := formatMagnitude(tc.in); got != tc.want {
			t.Errorf("formatMagnitude(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatCostDistinguishesZeroFromUnknown is the reason CostKnown exists:
// a fleet that has genuinely cost nothing and a fleet whose cost was never
// fetched must not render the same.
func TestFormatCostDistinguishesZeroFromUnknown(t *testing.T) {
	if got := formatCost(TokenCounts{CostUSD: 0, CostKnown: true}); got != "$0.00" {
		t.Errorf("known zero cost = %q, want $0.00", got)
	}
	if got := formatCost(TokenCounts{}); got != "—" {
		t.Errorf("unknown cost = %q, want an em dash", got)
	}
	if got := formatCost(usd(4.1)); got != "$4.10" {
		t.Errorf("cost = %q, want $4.10 (two decimals, not $4.1)", got)
	}
}

// TestTokensSortsBySpendDescending pins the display order. The dashboard keys
// usage by map, so without this the rows would reshuffle on every poll.
func TestTokensSortsBySpendDescending(t *testing.T) {
	msg := TokensMsg{Agents: []TokenRow{
		row("reviewer", 96_700, 12_400, 0.38),
		row("scanner", 1_200_000, 88_100, 4.10),
		row("quality", 410_300, 31_000, 1.32),
	}}

	next, _ := NewTokens().Update(msg)
	p, ok := next.(Tokens)
	if !ok {
		t.Fatalf("Update returned %T, want Tokens", next)
	}

	want := []string{"scanner", "quality", "reviewer"}
	if len(p.rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(p.rows), len(want))
	}
	for i, name := range want {
		if p.rows[i].Agent != name {
			t.Errorf("row %d = %q, want %q (descending by in+out)", i, p.rows[i].Agent, name)
		}
	}
}

// TestTokensSortIsTotalOnTies pins the name tie-break. Equal-spend agents that
// only sorted by spend would swap places between frames, which is the flicker
// the sort exists to prevent.
func TestTokensSortIsTotalOnTies(t *testing.T) {
	in := []TokenRow{row("zeta", 100, 100, 0), row("alpha", 100, 100, 0), row("mid", 100, 100, 0)}
	got := sortRows(in)
	for i, want := range []string{"alpha", "mid", "zeta"} {
		if got[i].Agent != want {
			t.Fatalf("tie-broken row %d = %q, want %q", i, got[i].Agent, want)
		}
	}
}

// TestSortRowsDoesNotMutateInput: the slice belongs to the message, and a pane
// that reordered it in place would scramble a payload the app may still hold.
func TestSortRowsDoesNotMutateInput(t *testing.T) {
	in := []TokenRow{row("b", 1, 1, 0), row("a", 9, 9, 0)}
	_ = sortRows(in)
	if in[0].Agent != "b" || in[1].Agent != "a" {
		t.Fatalf("sortRows reordered its input: %q, %q", in[0].Agent, in[1].Agent)
	}
}

// TestTokensPlaceholderUntilData pins that the pane says "waiting for data"
// only while that is true, and stops saying it once a message has arrived —
// including a message carrying no agents at all.
func TestTokensPlaceholderUntilData(t *testing.T) {
	if view := NewTokens().View(48, 12); !strings.Contains(view, placeholder) {
		t.Fatalf("pre-data view missing %q:\n%s", placeholder, view)
	}

	next, _ := NewTokens().Update(TokensMsg{})
	view := next.View(48, 12)
	if strings.Contains(view, placeholder) {
		t.Fatalf("view still shows %q after data arrived:\n%s", placeholder, view)
	}
	if !strings.Contains(view, "no usage recorded") {
		t.Fatalf("empty fleet view does not say so:\n%s", view)
	}
	if !strings.Contains(view, "total") {
		t.Fatalf("empty fleet view dropped the totals row:\n%s", view)
	}
}

// TestTokensTotalIsCarriedNotSummed is the correctness assertion behind
// TokensMsg.Total's doc comment: the dashboard's total counts sessions that
// may not map to a configured agent, so the pane must render what it was given
// even when that exceeds the sum of the rows.
func TestTokensTotalIsCarriedNotSummed(t *testing.T) {
	msg := TokensMsg{
		Agents: []TokenRow{row("scanner", 1_000, 1_000, 0.01)},
		Total:  TokenCounts{Input: 5_000_000, Output: 400_000, CostUSD: 12.34, CostKnown: true},
	}
	next, _ := NewTokens().Update(msg)
	view := next.View(60, 12)

	for _, want := range []string{"5.0M", "400.0k", "$12.34"} {
		if !strings.Contains(view, want) {
			t.Errorf("totals row missing %q — the pane re-summed its rows instead of using Total:\n%s", want, view)
		}
	}
}

// TestTokensUnknownCostRendersDash covers the shape the app will actually
// deliver until a cost fetch is wired: /api/tokens carries counts and no cost,
// so every row's CostKnown is false and the column must degrade rather than
// claim the fleet was free.
func TestTokensUnknownCostRendersDash(t *testing.T) {
	msg := TokensMsg{
		Agents: []TokenRow{{Agent: "scanner", TokenCounts: TokenCounts{Input: 1_200_000, Output: 88_100}}},
		Total:  TokenCounts{Input: 1_200_000, Output: 88_100},
	}
	next, _ := NewTokens().Update(msg)
	view := next.View(48, 12)

	if strings.Contains(view, "$0.00") {
		t.Fatalf("unfetched cost rendered as $0.00, which claims the fleet was free:\n%s", view)
	}
	if !strings.Contains(view, "—") {
		t.Fatalf("unfetched cost did not render as an em dash:\n%s", view)
	}
}

// TestTokensOverflowIsAnnounced pins that a pane too short for the fleet says
// how many rows it dropped. Silent truncation would read as a complete fleet
// whose rows do not add up to its own total.
func TestTokensOverflowIsAnnounced(t *testing.T) {
	var agents []TokenRow
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		agents = append(agents, row(name, 1000, 1000, 0.01))
	}
	next, _ := NewTokens().Update(TokensMsg{Agents: agents})

	// height 9 leaves 4 lines for agent rows, so 3 rows plus the notice.
	view := next.View(48, 9)
	if !strings.Contains(view, "+5 more") {
		t.Fatalf("short pane did not announce the dropped rows:\n%s", view)
	}
	if !strings.Contains(view, "total") {
		t.Fatalf("short pane dropped the totals row, which is the one line that must survive:\n%s", view)
	}
}

// TestTokensTooShortForRowsKeepsTotals: when not even one agent row fits, the
// totals are the more useful half and are what remain.
func TestTokensTooShortForRowsKeepsTotals(t *testing.T) {
	next, _ := NewTokens().Update(TokensMsg{
		Agents: []TokenRow{row("scanner", 1_200_000, 88_100, 4.10)},
		Total:  TokenCounts{Input: 1_200_000, Output: 88_100, CostUSD: 4.10, CostKnown: true},
	})
	view := next.View(48, 5)
	if !strings.Contains(view, "total") {
		t.Fatalf("totals row lost in a very short pane:\n%s", view)
	}
}

// TestTokensViewFillsItsBoxExactly is the grid's structural requirement: every
// pane renders exactly the size it was given, whatever its content, or the
// 2×2 join skews. Asserted across sizes because the row builder pads to a
// computed name column that a too-narrow pane would otherwise overrun.
func TestTokensViewFillsItsBoxExactly(t *testing.T) {
	next, _ := NewTokens().Update(TokensMsg{
		Agents: []TokenRow{
			row("scanner", 1_200_000, 88_100, 4.10),
			row("a-very-long-agent-name-indeed", 410_300, 31_000, 1.32),
			// Unknown cost too: the em dash and the ellipsis a truncated name
			// gets are both East-Asian-ambiguous runes, so the box arithmetic
			// has to hold with them present, not only with ASCII money.
			{Agent: "unpriced", TokenCounts: TokenCounts{Input: 900, Output: 40}},
		},
		Total: TokenCounts{Input: 1_610_300, Output: 119_100, CostUSD: 5.42, CostKnown: true},
	})

	for _, dims := range [][2]int{{48, 12}, {30, 8}, {80, 20}, {20, 6}} {
		w, h := dims[0], dims[1]
		view := next.View(w, h)
		if lines := strings.Count(view, "\n") + 1; lines != h {
			t.Errorf("View(%d,%d) rendered %d lines, want exactly %d", w, h, lines, h)
		}
		if vw := visibleWidth(view); vw != w {
			t.Errorf("View(%d,%d) widest line is %d cells, want exactly %d", w, h, vw, w)
		}
	}
}

// TestTokensDegeneratesSafely matches the other panes: a zero or negative box
// renders as nothing rather than emitting unpadded lines into the grid join.
func TestTokensDegeneratesSafely(t *testing.T) {
	next, _ := NewTokens().Update(TokensMsg{Agents: []TokenRow{row("scanner", 1, 1, 0)}})
	for _, dims := range [][2]int{{0, 5}, {5, 0}, {-1, -1}} {
		if got := next.View(dims[0], dims[1]); got != "" {
			t.Errorf("View(%d,%d) = %q, want empty", dims[0], dims[1], got)
		}
	}
}

// TestTokensIgnoresForeignMessages pins the T3 routing contract from the
// pane's side: every pane sees every non-key message, so a pane must not
// mistake another pane's payload for its own.
func TestTokensIgnoresForeignMessages(t *testing.T) {
	loaded, _ := NewTokens().Update(TokensMsg{
		Agents: []TokenRow{row("scanner", 1_200_000, 88_100, 4.10)},
		Total:  TokenCounts{Input: 1_200_000, Output: 88_100, CostUSD: 4.10, CostKnown: true},
	})
	before := loaded.View(48, 12)

	type otherPaneMsg struct{ Data string }
	after, cmd := loaded.Update(otherPaneMsg{Data: "not for you"})
	if cmd != nil {
		t.Error("a foreign message produced a command")
	}
	if got := after.View(48, 12); got != before {
		t.Errorf("a foreign message changed the pane:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestTokensKeysAreInert: the pane has no selection yet, so a key routed to it
// while focused must leave it untouched rather than silently doing something.
func TestTokensKeysAreInert(t *testing.T) {
	loaded, _ := NewTokens().Update(TokensMsg{Agents: []TokenRow{row("scanner", 1, 1, 0)}})
	before := loaded.View(48, 12)
	after, cmd := loaded.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if cmd != nil {
		t.Error("a key produced a command from a pane with no bindings")
	}
	if got := after.View(48, 12); got != before {
		t.Error("a key changed the pane")
	}
}

// TestTruncateClipsVisibly: a name cut to fit must show that it was cut, or an
// operator reads a truncated name as the whole name.
func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"scanner", 10, "scanner"},
		{"scanner", 7, "scanner"},
		{"scanner", 6, "scann…"},
		{"scanner", 1, "…"},
		{"scanner", 0, "scanner"},
	} {
		if got := truncate(tc.in, tc.width); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}
