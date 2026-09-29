package governor

import "time"

// SurgeDuration reports the current continuous, observed surge dwell. Missing
// history is unknown, not zero. It does not infer dwell across an unobserved
// restart or combine separate surge episodes.
func (g *Governor) SurgeDuration() (time.Duration, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.state.Mode != ModeSurge {
		return 0, true
	}
	if len(g.modeHistory) == 0 {
		return 0, false
	}
	change := g.modeHistory[len(g.modeHistory)-1]
	if change.To != ModeSurge || change.Timestamp.IsZero() || change.Timestamp.After(g.now()) {
		return 0, false
	}
	return g.now().Sub(change.Timestamp), true
}
