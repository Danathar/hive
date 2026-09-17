package dashboard

import (
	"fmt"
	"time"
)

const (
	// contributorFailureStreakThreshold is how many CONSECUTIVE fast failures
	// one identity books before its claims are paused. Mirrors the per-issue
	// consecutiveFailureQuarantineThreshold so "the same contributor keeps
	// insta-failing" trips at the same rate as "the same issue keeps failing".
	contributorFailureStreakThreshold = 3
	// contributorFastFailureMax is the assignment-to-failure duration at or
	// under which a failure reads as "the runtime died at startup" rather than
	// "the work was attempted". #6450's reported case failed in seconds.
	contributorFastFailureMax = 60 * time.Second
	// contributorFailureStreakPause is how long claims are refused once the
	// threshold trips, measured from the streak's most recent failure. Matches
	// the "you are in a 10-minute failure cooldown" shape proposed in #6450.
	contributorFailureStreakPause = 10 * time.Minute
)

// contributorFailureStreak is one identity's run of consecutive fast failures.
type contributorFailureStreak struct {
	// Count of consecutive sub-contributorFastFailureMax failures.
	Count int
	// LastAt is when the most recent counted failure was booked; the pause
	// window is measured from here.
	LastAt time.Time
	// LastReason is the relay-reported reason of the most recent counted
	// failure, kept only to make the refusal message concrete.
	LastReason string
}

// recordContributorFastFailure books one task_failed against the identity's
// streak. duration is hub-measured assignment-to-failure wall clock; zero or
// negative means "unknown" (adopted task) and is ignored. A slow failure
// resets the streak — see the file comment.
func (h *ContributeWSHub) recordContributorFastFailure(identity string, duration time.Duration, reason string) {
	if identity == "" || duration <= 0 {
		return
	}
	h.failureStreakMu.Lock()
	defer h.failureStreakMu.Unlock()
	if h.contributorFailureStreaks == nil {
		h.contributorFailureStreaks = make(map[string]contributorFailureStreak)
	}
	if duration > contributorFastFailureMax {
		delete(h.contributorFailureStreaks, identity)
		return
	}
	rec := h.contributorFailureStreaks[identity]
	rec.Count++
	rec.LastAt = time.Now()
	rec.LastReason = reason
	h.contributorFailureStreaks[identity] = rec
}

// resetContributorFailureStreak clears the identity's streak. Called on a
// genuine task_complete: the runtime demonstrably works.
func (h *ContributeWSHub) resetContributorFailureStreak(identity string) {
	if identity == "" {
		return
	}
	h.failureStreakMu.Lock()
	delete(h.contributorFailureStreaks, identity)
	h.failureStreakMu.Unlock()
}

// contributorFailureStreakActive reports whether the identity's claims are
// currently paused, and if so returns the streak count and the pause expiry.
// Entries whose pause has lapsed are left in place (not pruned) so a
// still-broken runtime's next fast failure re-arms the pause immediately.
func (h *ContributeWSHub) contributorFailureStreakActive(identity string, now time.Time) (bool, contributorFailureStreak, time.Time) {
	if identity == "" {
		return false, contributorFailureStreak{}, time.Time{}
	}
	h.failureStreakMu.Lock()
	defer h.failureStreakMu.Unlock()
	rec, ok := h.contributorFailureStreaks[identity]
	if !ok || rec.Count < contributorFailureStreakThreshold {
		return false, rec, time.Time{}
	}
	until := rec.LastAt.Add(contributorFailureStreakPause)
	if !now.Before(until) {
		return false, rec, time.Time{}
	}
	return true, rec, until
}

// contributorFailureStreakMessage renders the human-readable half of the
// refusal, naming the streak, the fast-failure bound, and the pause expiry so
// a relay operator reading their own log learns why work stopped arriving —
// the exact visibility gap #6450 reports.
func contributorFailureStreakMessage(rec contributorFailureStreak, until time.Time) string {
	msg := fmt.Sprintf(
		"your last %d assignments each failed within %s of assignment — this usually means the agent runtime is dying at startup (missing credential, misconfigured backend); check the relay's agent logs. Assignments paused until %s.",
		rec.Count, contributorFastFailureMax, until.UTC().Format(time.RFC3339))
	if rec.LastReason != "" {
		msg += " Last reported failure: " + rec.LastReason
	}
	return msg
}
