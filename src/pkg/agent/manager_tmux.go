// The tmux terminal seams: session creation/existence, pane capture
// (visible, scrollback, full log), literal/key sends, CLI/input-prompt
// readiness waits, and tmux argv plumbing. These are the eight terminal
// seam methods #5638 names for the TerminalSession extraction.
package agent

import (
	"time"
)

// attachedClientIdleGrace is how long a tmux client may sit with no activity
// at all before SessionAttached stops counting it as a human at the keyboard.
//
// SessionAttached exists so hive never types into a session someone is using.
// It asked tmux only HOW MANY clients were attached, and a client that dies
// without detaching -- an SSH drop, a closed `kubectl exec`, a browser tab shut
// on a terminal view -- stays attached forever. On a hosted spoke two such
// clients sat on the scanner session with client_activity equal to
// client_created for 13.5 hours, and no process owning either pty. Every heal
// that routes through this guard was disabled for that agent the entire time:
// scanner hit a transient model error, dropped to an idle prompt, and sat
// there because the retry nudge kept vetoing itself on a terminal nobody was
// looking at.
//
// The grace is deliberately generous. A human reading output without typing
// still trips it eventually, and the cost of being wrong in that direction is
// one "try again" typed into a pane they are watching -- against an agent
// stalled indefinitely if we are wrong in the other.
const attachedClientIdleGrace = 30 * time.Minute

func (t tmuxTerminal) SleepDuringPromptDismiss(d time.Duration) {
	time.Sleep(d)
}
