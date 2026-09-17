package agent

// KickObserverEventDelivered / KickObserverEventArchived name the two events.
const (
	KickObserverEventDelivered = "kick-delivered"
	KickObserverEventArchived  = "kick-log-archived"
)

// SetKickObserver installs (or with nil, removes) the kick lifecycle
// observer. Safe to leave unset: notifications are then no-ops.
func (m *Manager) SetKickObserver(fn func(agentName, event, detail string)) {
	if fn == nil {
		m.kickObserver.Store(nil)
		return
	}
	m.kickObserver.Store(&fn)
}

// notifyKickObserver dispatches one event asynchronously. Callers may hold
// m.mu; the observer never runs under it.
func (m *Manager) notifyKickObserver(agentName, event, detail string) {
	fn := m.kickObserver.Load()
	if fn == nil {
		return
	}
	go (*fn)(agentName, event, detail)
}
