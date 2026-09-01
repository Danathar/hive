package tui

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const tmuxSessionPrefix = "hive-"

// tmuxNotFoundError distinguishes an unavailable local tmux binary from a
// session lookup failure. Both are operator-facing footer errors, but keeping
// the causes typed lets callers and tests reason about them without matching
// prose.
type tmuxNotFoundError struct {
	err error
}

func (e *tmuxNotFoundError) Error() string {
	return "tmux is not installed or is not available in PATH"
}

func (e *tmuxNotFoundError) Unwrap() error { return e.err }

// tmuxSessionMissingError is returned when tmux cannot find the selected
// agent's session during the preflight. tmux uses the same non-zero result for
// a missing server and a missing session; both mean the local attach target is
// unavailable, and the diagnostic text preserves that distinction for the
// operator when tmux supplies it.
type tmuxSessionMissingError struct {
	session string
	detail  string
	err     error
}

func (e *tmuxSessionMissingError) Error() string {
	message := fmt.Sprintf("tmux session %q is unavailable", e.session)
	if e.detail != "" {
		message += ": " + e.detail
	}
	return message
}

func (e *tmuxSessionMissingError) Unwrap() error { return e.err }

// attachReadyMsg is the result of checking the local tmux target. The check is
// a regular tea.Cmd so it does not block the TUI's update loop; only a proven
// attach command is handed to tea.ExecProcess, which suspends the terminal.
type attachReadyMsg struct {
	cmd *exec.Cmd
	err error
}

// attachDoneMsg is delivered after Bubble Tea has restored the terminal. A
// nil error means tmux exited normally; either way the app refreshes the fleet
// because its state may have changed while the operator was attached.
type attachDoneMsg struct {
	err error
}

func tmuxSessionFor(agent string) string {
	return tmuxSessionPrefix + agent
}

// attachCmdFor constructs, but does not run, the interactive command for an
// agent. exec.Command resolves tmux through PATH while retaining "tmux" as
// Args[0], so the command is both directly executable and easy to audit as
// exactly `tmux attach -t hive-<agent>`.
func attachCmdFor(agent string) (*exec.Cmd, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, &tmuxNotFoundError{err: err}
	}
	return exec.Command("tmux", "attach", "-t", tmuxSessionFor(agent)), nil
}

// prepareAttach checks the session before Bubble Tea releases the terminal.
// Running attach blindly would briefly leave the alternate screen just to
// print tmux's expected missing-session error, then redraw the TUI. Preflight
// keeps that failure in-band so Update can show it in the footer instead.
func prepareAttach(agent string) tea.Cmd {
	return func() tea.Msg {
		cmd, err := attachCmdFor(agent)
		if err != nil {
			return attachReadyMsg{err: err}
		}

		session := tmuxSessionFor(agent)
		check := exec.Command(cmd.Path, "has-session", "-t", session)
		output, err := check.CombinedOutput()
		if err != nil {
			// tmux diagnostics are one or two short stderr lines. Folding them
			// keeps the footer a single line even when a platform emits both.
			detail := strings.Join(strings.Fields(string(output)), " ")
			return attachReadyMsg{err: &tmuxSessionMissingError{
				session: session,
				detail:  detail,
				err:     err,
			}}
		}
		return attachReadyMsg{cmd: cmd}
	}
}
