package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
)

// finalWait bounds every WaitFinished. Generous enough that a loaded CI runner
// does not flake, short enough that a model which never quits fails the test
// instead of hanging until the suite-level timeout.
const finalWait = 5 * time.Second

// TestAppQuits is the scaffold's contract: `q` ends the program cleanly.
//
// It drives the real model through teatest rather than calling Update directly,
// so it exercises the whole bubbletea loop — input decoding, the Update return,
// and the tea.Quit command actually terminating the program. A unit test on
// Update alone would still pass if tea.Quit were never returned as a command.
func TestAppQuits(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})

	// WaitFinished fails the test if the program is still running at the
	// deadline, so reaching the next line IS the clean-exit assertion.
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestAppQuitsOnCtrlC covers the other documented quit binding. ctrl+c travels
// a different path through bubbletea than a plain rune, so `q` passing says
// nothing about it.
func TestAppQuitsOnCtrlC(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestAppRendersGrid pins what a sized frame draws since T3: all four pane
// titles, on screen at once. Asserting on the titles rather than a golden
// file keeps this test stable across layout refinement — the full frame's
// exact bytes are pinned separately by the golden test in pkg/tui/panes.
func TestAppRendersGrid(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, title := range []string{"AGENTS", "GOVERNOR", "TOKENS", "EVENTS"} {
			if !strings.Contains(string(b), title) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(finalWait))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestTabCyclesFocus drives tab and shift+tab through the real program, per
// the T3 acceptance criteria. teatest rather than bare Update calls for the
// same reason as TestAppQuits: input decoding is part of what is asserted —
// shift+tab in particular arrives as its own key sequence, and a handler
// matching the wrong spelling would pass a unit test that hand-builds the
// message it expects.
func TestTabCyclesFocus(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	// Two tabs forward, one back: ends on pane 1 iff both directions work
	// and neither is a no-op. (2-0 would also pass if shift+tab did nothing
	// and one tab was dropped, but then the direct cycle test below fails.)
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyShiftTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

	final, ok := tm.FinalModel(t).(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
	}
	if final.focus != 1 {
		t.Fatalf("after tab,tab,shift+tab focus = %d, want 1", final.focus)
	}
}

// TestFocusCycleWrapsBothDirections pins the modulo arithmetic directly:
// forward from the last pane wraps to the first, and backward from the first
// wraps to the last — the negative-operand case the "+paneCount-1" spelling
// exists for.
func TestFocusCycleWrapsBothDirections(t *testing.T) {
	m := newModel()
	for i := 0; i < paneCount; i++ {
		if m.focus != i {
			t.Fatalf("tab cycle step %d: focus = %d, want %d", i, m.focus, i)
		}
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(model)
	}
	if m.focus != 0 {
		t.Fatalf("tab from last pane: focus = %d, want wrap to 0", m.focus)
	}
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := back.(model).focus; got != paneCount-1 {
		t.Fatalf("shift+tab from pane 0: focus = %d, want wrap to %d", got, paneCount-1)
	}
}

// TestFocusHighlightsExactlyOnePane pins the focused-border contract from the
// issue ("the focused pane gets a highlighted border") in a way no golden
// file drift can silently weaken: exactly one cell wears the thick border,
// and tab moves it.
func TestFocusHighlightsExactlyOnePane(t *testing.T) {
	sized, _ := newModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := sized.(model)

	// The thick border's top-left corner only appears on the focused cell.
	const thickCorner = "┏"
	if got := strings.Count(m.View(), thickCorner); got != 1 {
		t.Fatalf("frame has %d thick-border corners, want exactly 1 (one focused pane)", got)
	}
	before := m.View()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	after := next.(model).View()
	if before == after {
		t.Fatal("tab did not move the focus highlight: frame unchanged")
	}
	if got := strings.Count(after, thickCorner); got != 1 {
		t.Fatalf("after tab, frame has %d thick-border corners, want exactly 1", got)
	}
}

// TestViewBeforeSizeMsg guards the zero-size path in View. bubbletea can call
// View before the first tea.WindowSizeMsg arrives; centering into a 0x0 box
// renders nothing, so an unsized frame would flash blank rather than telling
// the operator how to get out.
func TestViewBeforeSizeMsg(t *testing.T) {
	if got := newModel().View(); got != splash {
		t.Fatalf("unsized View() = %q, want %q", got, splash)
	}
}

// TestViewFillsTerminalExactly checks the grid arithmetic: the frame is
// exactly as tall as the terminal (header + two grid rows + footer), and no
// rendered line overflows the width — the remainder-absorbing split, off by
// one, would do both.
func TestViewFillsTerminalExactly(t *testing.T) {
	const w, h = 80, 24
	m, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})

	view := m.(model).View()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Fatalf("sized View() renders %d lines, want exactly %d", len(lines), h)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw > w {
			t.Fatalf("line %d is %d cells wide, want <= %d:\n%q", i, lw, w, line)
		}
	}
	for _, want := range []string{headerText, footerText} {
		if !strings.Contains(view, want) {
			t.Fatalf("sized View() missing %q", want)
		}
	}
}

// TestUnhandledKeyDoesNotQuit pins that the quit set is exactly the documented
// one. Without this, a `default: return m, tea.Quit` slip would leave every
// other test green while making the TUI exit on any keypress.
func TestUnhandledKeyDoesNotQuit(t *testing.T) {
	_, cmd := newModel().Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Fatal("an unbound key returned a command; only q and ctrl+c should quit")
	}
}

// TestRunOverPipes drives the real program — the constructor Run uses, options
// and all — with a pipe standing in for the terminal.
//
// The teatest cases above build their own program internally, so none of them
// executes tea.NewProgram in app.go. Without this, WithAltScreen could be
// dropped and every other test would still pass, while `hivectl tui` would
// start scribbling over the operator's scrollback instead of taking its own
// screen.
func TestRunOverPipes(t *testing.T) {
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- run(bytes.NewReader([]byte("q")), &out) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
	case <-time.After(finalWait):
		t.Fatal("run() did not return after q on stdin")
	}

	rendered := out.String()
	if !strings.Contains(rendered, splash) {
		t.Fatalf("run() output does not contain %q:\n%q", splash, rendered)
	}
	// ESC[?1049h / ESC[?1049l are the alt-screen enter/leave pair. Asserting
	// BOTH is the point: entering without leaving is the failure that strands
	// an operator's terminal in the alternate buffer after exit.
	for _, want := range []string{"\x1b[?1049h", "\x1b[?1049l"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("run() output missing alt-screen sequence %q", want)
		}
	}
}
