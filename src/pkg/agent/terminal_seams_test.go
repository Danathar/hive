package agent

import "time"

// funcTerminal is the test-side TerminalSession: any nil method func falls
// back to the real tmux-backed implementation, preserving the exact
// semantics of the pre-#5636 per-field seams (an unset seam reached tmux).
type funcTerminal struct {
	base TerminalSession

	capturePane        func(agent *AgentProcess) string
	captureVisiblePane func(agent *AgentProcess) string
	sessionAttached    func(agent *AgentProcess) bool
	sendLiteral        func(agent *AgentProcess, text string)
	sendKeys           func(agent *AgentProcess, keys ...string)
	sleep              func(d time.Duration)
	captureFullLog     func(agent *AgentProcess) (string, error)
	clearHistory       func(agent *AgentProcess)
}

// termSeams returns the Manager's funcTerminal, installing one (backed by
// the real tmux implementation) on first use. Tests set individual method
// funcs on the returned value, mirroring the old field assignments.
func termSeams(m *Manager) *funcTerminal {
	if ft, ok := m.terminal.(*funcTerminal); ok {
		return ft
	}
	ft := &funcTerminal{base: tmuxTerminal{m: m}}
	m.terminal = ft
	return ft
}

func (f *funcTerminal) CapturePane(agent *AgentProcess) string {
	if f.capturePane != nil {
		return f.capturePane(agent)
	}
	return f.base.CapturePane(agent)
}

func (f *funcTerminal) CaptureVisiblePane(agent *AgentProcess) string {
	if f.captureVisiblePane != nil {
		return f.captureVisiblePane(agent)
	}
	return f.base.CaptureVisiblePane(agent)
}

func (f *funcTerminal) SessionAttached(agent *AgentProcess) bool {
	if f.sessionAttached != nil {
		return f.sessionAttached(agent)
	}
	return f.base.SessionAttached(agent)
}

func (f *funcTerminal) SendLiteral(agent *AgentProcess, text string) {
	if f.sendLiteral != nil {
		f.sendLiteral(agent, text)
		return
	}
	f.base.SendLiteral(agent, text)
}

func (f *funcTerminal) SendKeys(agent *AgentProcess, keys ...string) {
	if f.sendKeys != nil {
		f.sendKeys(agent, keys...)
		return
	}
	f.base.SendKeys(agent, keys...)
}

func (f *funcTerminal) Sleep(d time.Duration) {
	if f.sleep != nil {
		f.sleep(d)
		return
	}
	f.base.Sleep(d)
}

func (f *funcTerminal) CaptureFullLog(agent *AgentProcess) (string, error) {
	if f.captureFullLog != nil {
		return f.captureFullLog(agent)
	}
	return f.base.CaptureFullLog(agent)
}

func (f *funcTerminal) ClearHistory(agent *AgentProcess) {
	if f.clearHistory != nil {
		f.clearHistory(agent)
		return
	}
	f.base.ClearHistory(agent)
}
