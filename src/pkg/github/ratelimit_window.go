package github

import (
	"sync"
	"time"
)

// Rate-limit readings are clamped to be monotone WITHIN a window
// (kubestellar/hive#5733).
//
// THE BUG. `/api/status` → .ghRateLimits.core.remaining intermittently reported
// the full limit while most of the GitHub App installation's budget was spent —
// and it moved BACKWARDS, reporting more headroom than a minute earlier. Paired
// samples, one per minute, same installation: hive's value against a direct
// GET /rate_limit with a token minted from the same App key in the same pod.
//
//	20:56  hive 6941   actual 6815
//	20:57  hive 7100   actual 6741   <- UP, while real usage climbed
//	21:02  hive 7100   actual 6286   <- pinned at the limit for six minutes
//
// Peak divergence was 814 requests: the card claimed 100% headroom at ~89%
// actual. An operator reads that card to decide whether there is room to run
// agents harder, so a false-full reading throttles or scales the wrong thing —
// which is exactly what happened to the reporter.
//
// THE MECHANISM. An installation access token reports a fresh, EMPTY bucket
// transiently right after minting, even though the budget is genuinely shared
// across every token for that installation. Demonstrated directly, seconds
// apart on one installation:
//
//	token A initial: {"used":90,  "remaining":7010, "reset":1788385930}
//	  ... 20 requests issued with token A ...
//	token A after:   {"used":0,   "remaining":7100, "reset":1788386085}
//	token B (fresh): {"used":160, "remaining":6940, "reset":1788385930}
//
// Token B proves the bucket IS shared — it sees A's 20 requests. Token A's own
// read is what goes empty. So whenever the client behind RateLimits() has
// recently re-minted, the dashboard latched that empty reading.
//
// WHY "RESET ADVANCED ⇒ NEW WINDOW" IS NOT ENOUGH. That is the obvious way to
// write this clamp, and the trace above defeats it: the bogus reading carries a
// LATER reset (1788386085 vs 1788385930), because a fresh bucket reports its
// own fresh window. A clamp keyed on the reset value alone would read every
// re-mint as a rollover and accept exactly the readings it exists to reject —
// and the reporter measured that adrift reset directly ("at 20:57 the card's
// reset was 8.5 minutes adrift of B's").
//
// So a window is anchored on WALL-CLOCK EXPIRY instead: a new window is only
// believed once the previous window's reset has actually passed. Before then, a
// reading carrying a different reset is a re-minted token describing its own
// bucket, and is discarded.
//
// FAILURE DIRECTION. Every branch here fails toward reporting LESS headroom
// than may really be available: a clamped value is the lowest seen this window,
// and a rejected reading leaves the previous one standing. That is deliberate.
// Under-reporting headroom costs some throughput; over-reporting it is what
// produced this issue. A clock running behind GitHub's can hold a window past
// its true rollover, which costs at most one window of stale-low display and
// self-corrects.

// rateLimitWindow is the last ACCEPTED observation for one bucket.
type rateLimitWindow struct {
	limit int
	// remaining is the LOWEST remaining seen during this window — the whole
	// point of the clamp, since within a window the real value only falls.
	remaining int
	reset     time.Time
	// observedAt is when the reported remaining was actually observed. It stops
	// advancing while a reading is held, which is what makes a stale value
	// visibly stale on the dashboard instead of silently confident.
	observedAt time.Time
}

// rateLimitTracker holds one window per bucket ("core", "search", "graphql").
// The zero value is ready to use.
type rateLimitTracker struct {
	mu      sync.Mutex
	windows map[string]rateLimitWindow
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

func (t *rateLimitTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// observe folds one raw reading into the tracked window and returns the value
// that should be reported.
//
// An empty entry (no limit, no remaining, no reset) means the API did not
// report that bucket at all; it is passed straight through and never tracked,
// so an absent bucket cannot pin a window.
func (t *rateLimitTracker) observe(bucket string, obs RateLimitEntry) RateLimitEntry {
	if obs.Limit == 0 && obs.Remaining == 0 && obs.Reset.IsZero() {
		return obs
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.windows == nil {
		t.windows = make(map[string]rateLimitWindow, 3)
	}

	now := t.clock()
	prev, tracked := t.windows[bucket]

	switch {
	case !tracked:
		// Nothing to compare against; the first reading establishes the window.

	case !now.Before(prev.reset):
		// The previous window's reset has actually passed, so this is a genuine
		// rollover and the budget really has been restored. Wall-clock expiry —
		// not a changed reset value — is what proves it.

	case obs.Reset.Equal(prev.reset):
		// Same window. The true remaining only falls within a window, so a
		// higher reading is not new information; it is the fresh-bucket
		// artifact. Keep the lowest seen.
		if obs.Remaining >= prev.remaining {
			return entryOf(prev)
		}

	default:
		// A different reset while the previous window is still open: a re-minted
		// token describing its own empty bucket, carrying its own later reset.
		// This is the exact reading that produced #5733; discard it.
		return entryOf(prev)
	}

	accepted := rateLimitWindow{
		limit:      obs.Limit,
		remaining:  obs.Remaining,
		reset:      obs.Reset,
		observedAt: now,
	}
	t.windows[bucket] = accepted
	return entryOf(accepted)
}

func entryOf(w rateLimitWindow) RateLimitEntry {
	return RateLimitEntry{
		Limit:      w.limit,
		Remaining:  w.remaining,
		Reset:      w.reset,
		ObservedAt: w.observedAt,
	}
}
