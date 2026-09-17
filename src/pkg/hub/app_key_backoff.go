package hub

import (
	"time"
)

const (
	// appKeyDeliveryImmediateBudget is how many consecutive key deliveries to
	// one hive stay immediate (every beat) before the hub concludes delivery
	// is not converging. Both delivery paths — the primary cluster-key
	// reconcile and the additional-keys pass — share the counter, so a spoke
	// broken on both reaches the budget in about half as many beats; at a 2-
	// minute beat cadence the budget spans roughly 6-12 minutes either way,
	// ample for any spoke that is actually applying what it receives.
	appKeyDeliveryImmediateBudget = 6
	// appKeyDeliveryBackoffInterval is how often key material is still
	// delivered once the budget is exhausted. Long enough to stop the
	// every-beat hot loop; short enough that a spoke that recovers (restart,
	// upgrade) is re-keyed within minutes.
	appKeyDeliveryBackoffInterval = 10 * time.Minute
)

// appKeyDeliveryState is the per-hive delivery ledger.
type appKeyDeliveryState struct {
	// consecutive counts key deliveries since the last observed convergence.
	consecutive int
	// lastSent is when key material last actually rode a beat.
	lastSent time.Time
}

// allowAppKeyDelivery reports whether key material may ride this beat for the
// hive. Inside the immediate budget it is always true; past it, true only once
// per appKeyDeliveryBackoffInterval, with a WARN naming the hive and the
// consecutive-delivery count so the non-convergence is legible in hub logs
// instead of silently re-sending key material forever.
func (s *HubServer) allowAppKeyDelivery(hiveID string) bool {
	s.appKeyDeliveryMu.Lock()
	defer s.appKeyDeliveryMu.Unlock()
	st := s.appKeyDelivery[hiveID]
	if st == nil || st.consecutive < appKeyDeliveryImmediateBudget {
		return true
	}
	if time.Since(st.lastSent) >= appKeyDeliveryBackoffInterval {
		if s.logger != nil {
			s.logger.Warn("heartbeat: app key delivery to this hive is NOT converging — spoke never reflects delivered keys; throttled to one delivery per backoff interval",
				"hive_id", hiveID,
				"consecutive_deliveries", st.consecutive,
				"backoff", appKeyDeliveryBackoffInterval.String(),
				"hint", "the spoke is not persisting or not reporting delivered key material — check for a duplicate spoke process (#2496) or a /data write failure",
			)
		}
		return true
	}
	return false
}

// noteAppKeyDelivered records that key material rode a beat to this hive.
func (s *HubServer) noteAppKeyDelivered(hiveID string) {
	s.appKeyDeliveryMu.Lock()
	defer s.appKeyDeliveryMu.Unlock()
	if s.appKeyDelivery == nil {
		s.appKeyDelivery = make(map[string]*appKeyDeliveryState)
	}
	st := s.appKeyDelivery[hiveID]
	if st == nil {
		st = &appKeyDeliveryState{}
		s.appKeyDelivery[hiveID] = st
	}
	st.consecutive++
	st.lastSent = time.Now()
}

// noteAppKeyConverged clears the hive's delivery ledger: the spoke's report
// shows delivery landed (or nothing is pending), so future deliveries are
// immediate again.
func (s *HubServer) noteAppKeyConverged(hiveID string) {
	s.appKeyDeliveryMu.Lock()
	defer s.appKeyDeliveryMu.Unlock()
	delete(s.appKeyDelivery, hiveID)
}
