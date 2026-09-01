package hub

import (
	"os"
	"strconv"
	"time"
)

// Merge-driven upgrade debounce (#5391).
//
// THE DEFECT THIS FIXES
//
// Instant mode fires the moment a hive is seen behind latest. `latestSHA` is
// re-resolved every latestSHAPollInterval, so on a busy branch the hive is seen
// behind again a couple of minutes after it lands, and it rolls again. The rate
// is therefore set by MERGE FREQUENCY, an input with nothing to do with how
// urgently a spoke needs the image. Measured on
// hive-hosted-hosted-available-oke-11-placeholder-r05x: ELEVEN ReplicaSets in
// 5.5 hours, each stamped with a distinct upgrade-target-sha matching a v4
// merge. Each roll kills every contributor WebSocket (#5090), spends ~2m05s
// pulling a 4.05 GB image, and interrupts in-flight agent work.
//
// Every individual roll was correct. Only the rate was wrong.
//
// WHY DEBOUNCE RATHER THAN A MINIMUM INTERVAL OR A WINDOW
//
// A burst of merges wants to collapse to ONE roll at the NEWEST SHA — which is
// also the SHA you actually want to be running. Debounce gives exactly that: a
// newer target arriving inside the quiet window REPLACES the pending one
// instead of queuing a second roll, and the roll fires once the branch has been
// quiet for autoUpgradeDebounceInterval. A minimum-interval gate would also cut
// the rate, but it rolls at whatever target happened to be armed when the timer
// expired rather than converging on the newest; a scheduled window (the daily
// and weekly modes below) is far too coarse for a hive that wants to track a
// branch.
//
// RELATIONSHIP TO shouldAutoUpgradeNow
//
// This is deliberately the SAME SHAPE as the daily/weekly gate in
// upgrade_schedule.go and reuses its decision type, so there is one vocabulary
// for "may this hive fire now". It could not simply reuse
// shouldAutoUpgradeNow's fired-date gate, because that gate answers "have we
// already fired in this window" from a calendar day, and debounce must answer
// "has the TARGET stopped moving" from a target plus a timestamp. The two
// compose rather than compete: debounce gates instant mode only, and
// shouldAutoUpgradeNow keeps gating daily and weekly, which are already far
// coarser than any debounce interval and so have nothing to gain from one.
//
// It does inherit upgrade_schedule.go's most important rule directly. Missed
// windows do NOT accumulate there, because "only ONE upgrade is ever owed" —
// the gate is a state, not a queue. Debounce holds exactly the same line: at
// most ONE target is ever pending, N merges inside a window collapse into one
// roll, and the collapsed count is reported rather than silently discarded.

// defaultAutoUpgradeDebounceInterval is how long a branch must be quiet before a
// merge-driven upgrade is allowed to roll.
//
// Five minutes is chosen against the two costs it sits between. Below it is the
// merge cadence being absorbed: the measured burst ran 11 merges in 5.5 hours,
// with the tightest pair 16 minutes apart and several inside 25 — but the
// expensive case is back-to-back merges (a PR train, a revert chasing a fix),
// which land within a few minutes of each other and are precisely what must
// collapse. Above it is the delay added to a QUIET branch, which is the only
// case debounce makes worse: a single merge with nothing behind it now waits
// this long before rolling. Five minutes is comfortably longer than a
// back-to-back merge pair and comfortably shorter than the ~2m05s image pull
// plus rollout it is protecting, so a lone merge still lands promptly while a
// train collapses to one roll.
//
// It is deliberately NOT tied to latestSHAPollInterval (2m). Coupling them
// would silently re-tune the debounce whenever the poll cadence changed for
// unrelated reasons.
const defaultAutoUpgradeDebounceInterval = 5 * time.Minute

// defaultAutoUpgradeMaxHold bounds how long a single pending upgrade may be
// deferred by repeated re-arming, regardless of how busy the branch is.
//
// This is not belt-and-braces; it is required by the MEASURED cadence of the
// branch being debounced. Sampling the last 100 merges to v4 (2026-08-30 19:07Z
// → 2026-08-31 23:33Z): the MEDIAN inter-merge gap is 3.0 minutes, 63% of gaps
// are under 5 minutes, and the longest observed run of consecutive sub-5-minute
// gaps is 13 — which a pure debounce would turn into a ~50-minute hold, and on a
// busier day an unbounded one.
//
// A pure debounce is therefore the wrong shape on its own here: "wait for quiet"
// assumes quiet arrives, and on this branch it frequently does not. The max hold
// converts the guarantee from "rolls once the branch goes quiet" into "rolls
// once the branch goes quiet, and in no case later than this" — which is what
// makes the change safe to run on a branch whose median gap is below the
// debounce interval.
//
// 30 minutes is chosen to sit well above the interval (so it never pre-empts an
// ordinary quiet-window roll) while still being far below the 5.5-hour window in
// which the incident produced 11 rolls. Worst case the fleet now rolls about
// twice an hour instead of every merge.
const defaultAutoUpgradeMaxHold = 30 * time.Minute

// autoUpgradeDebounceInterval resolves the live debounce interval. The
// dashboard-saved Scale Controls value wins, then HIVE_UPGRADE_DEBOUNCE_SECONDS,
// then the built-in default — the same precedence as upgradeWaveSize(), and read
// on every trigger tick so a saved change takes effect without a hub restart.
//
// A NEGATIVE value at either layer DISABLES debounce and restores the historical
// roll-on-every-merge behaviour. That escape hatch is deliberate: if debouncing
// ever turns out to hide an upgrade, an operator can turn it off fleet-wide
// without shipping a build. Zero is "not set" (the omitempty JSON default and an
// unset env var), NOT "disabled" — otherwise every existing settings document,
// which has no such field, would silently disable the feature.
//
// This deliberately does NOT use settingOrEnv/envInt. Both clamp to >= 1 and
// fall through to the default on anything smaller, so routing through them would
// make the documented "disable" value silently mean "use the default" — the
// escape hatch would look configured and do nothing.
func autoUpgradeDebounceInterval() time.Duration {
	secs := getScaleSettings().UpgradeDebounceSeconds
	if secs == 0 {
		if v := os.Getenv("HIVE_UPGRADE_DEBOUNCE_SECONDS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				secs = n
			}
		}
	}
	if secs < 0 {
		return 0 // explicitly disabled
	}
	if secs == 0 {
		return defaultAutoUpgradeDebounceInterval
	}
	return time.Duration(secs) * time.Second
}

// autoUpgradeMaxHold resolves the live max-hold cap, overridable via
// HIVE_UPGRADE_MAX_HOLD_SECONDS on the same conventions as the debounce
// interval: 0 = use the default, negative = no cap at all.
//
// Disabling the cap is deliberately possible but is NOT recommended on a branch
// whose median inter-merge gap is below the debounce interval — see
// defaultAutoUpgradeMaxHold for the measurement.
func autoUpgradeMaxHold() time.Duration {
	secs := 0
	if v := os.Getenv("HIVE_UPGRADE_MAX_HOLD_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	if secs < 0 {
		return 0 // cap explicitly disabled
	}
	if secs == 0 {
		return defaultAutoUpgradeMaxHold
	}
	return time.Duration(secs) * time.Second
}

// autoUpgradeDebounceState is the per-hive pending-target record.
//
// It is persisted in the hive's own meta.json (see SaaSHive) rather than held in
// memory, because a hub restart inside the quiet window must not DROP the
// pending upgrade. Losing an upgrade silently is a worse failure than rolling
// too often — the whole point of this change is that the hive still converges on
// latest, just less often. On restart the state is re-read and the window
// resumes from the original ArmedAt, so a restart neither loses the target nor
// extends the wait indefinitely.
type autoUpgradeDebounceState struct {
	// Target is the newest SHA seen for this hive's branch while waiting.
	Target string
	// ArmedAt is when the CURRENT pending target was first observed. It is
	// deliberately reset each time a NEWER target replaces the pending one:
	// that is what makes the window measure "the branch has been quiet",
	// not "we have been waiting a while".
	ArmedAt time.Time
	// FirstArmedAt is when this hive FIRST fell behind and began waiting, and
	// unlike ArmedAt it survives re-arming. It is what the max-hold cap is
	// measured against: without it, a branch that never goes quiet would reset
	// the only clock there was and defer the upgrade forever.
	FirstArmedAt time.Time
	// Collapsed counts how many distinct targets have superseded one another
	// inside this window. It exists so the eventual roll can REPORT how many
	// merges it absorbed. Silent batching would trade one invisible problem
	// for another (see #5388 on guards that cannot report).
	Collapsed int
}

// autoUpgradeDebounceDecision is the result of evaluating one hive's debounce
// state for one poll cycle.
type autoUpgradeDebounceDecision struct {
	// Allowed reports whether the roll may proceed NOW.
	Allowed bool
	// State is the debounce state to persist for this hive. It is meaningful
	// whether or not the roll is allowed: when held it carries the (possibly
	// just-replaced) pending target, and when allowed it is the zero value,
	// which clears the record.
	State autoUpgradeDebounceState
	// Collapsed is how many merges this roll absorbed beyond the first. Zero
	// means the target never moved while waiting. Only meaningful when Allowed.
	Collapsed int
	// Reason is a short, log-friendly explanation. Always populated.
	Reason string
}

// shouldDebounceAutoUpgrade decides whether a merge-driven upgrade to target may
// roll now, or must wait for the branch to go quiet.
//
// prev is the hive's stored debounce state (zero value if none). target is the
// SHA the hive would upgrade to on this cycle. interval is the quiet period; a
// non-positive interval disables debouncing entirely. maxHold caps the total
// time a pending upgrade may be deferred by repeated re-arming, so a branch
// that never goes quiet still converges; a non-positive maxHold disables the
// cap. now is injected so the
// behaviour is testable without touching the wall clock.
//
// The three cases:
//
//	NEW TARGET     — nothing pending, or a target NEWER than the pending one.
//	                 Arm (or re-arm) the window and hold. Re-arming is what
//	                 collapses a burst: the pending target is REPLACED, never
//	                 queued, so N merges still produce exactly one roll.
//	SAME TARGET    — the branch has not moved since we armed. Roll once the
//	                 window has elapsed; keep holding until it has.
//	NO TARGET      — nothing to do; clear any stale pending state.
//
// A hub restart inside the window is handled by prev being loaded from disk:
// the SAME-TARGET case then sees the original ArmedAt and fires on schedule
// rather than restarting the clock.
func shouldDebounceAutoUpgrade(prev autoUpgradeDebounceState, target string, interval, maxHold time.Duration, now time.Time) autoUpgradeDebounceDecision {
	if target == "" {
		// Nothing to arm. Clear any pending record so a stale target cannot
		// later fire against a branch that has since moved on.
		return autoUpgradeDebounceDecision{Reason: "no target"}
	}
	if interval <= 0 {
		// Debounce disabled — historical behaviour, roll immediately.
		return autoUpgradeDebounceDecision{Allowed: true, Reason: "debounce disabled"}
	}

	// A target that differs from the pending one supersedes it. sameCommit
	// tolerates the short/full SHA length mismatch the rest of the upgrade path
	// already handles, so a hub storing a 7-char SHA and a spoke reporting a
	// longer one do not look like a fresh merge on every single cycle — which
	// would re-arm the window forever and never roll at all.
	if prev.Target == "" || !sameCommit(prev.Target, target) {
		collapsed := prev.Collapsed
		reason := "debounce armed — waiting for the branch to go quiet"
		firstArmed := prev.FirstArmedAt
		if prev.Target != "" {
			// A newer target REPLACED a pending one. This is the collapse.
			collapsed++
			reason = "debounce re-armed — newer target supersedes the pending one"
		}
		// A record with no live pending target cannot contribute a wait clock.
		// prev.Target == "" means the previous cycle was not actually holding
		// anything — the hive caught up by another route (a manual upgrade, a
		// channel switch) and the target was consumed without this gate
		// clearing the record. Inheriting FirstArmedAt from it would make the
		// cap read "already waited hours" and fire on the FIRST cycle of a
		// genuinely new merge, silently skipping the debounce for it.
		//
		// This is keyed on PROVENANCE, not on elapsed time. An earlier attempt
		// treated "older than maxHold" as the staleness signal, which is wrong
		// and dangerous: on a branch whose merge gap exceeds the poll step the
		// reset fires before the cap can, pushing the clock forward every cycle
		// so the cap NEVER fires and the hive starves completely (measured: a
		// 7-minute merge cadence produced ZERO rolls in 4.5 hours).
		if firstArmed.IsZero() || prev.Target == "" {
			firstArmed = now
		}
		next := autoUpgradeDebounceState{
			Target: target, ArmedAt: now, FirstArmedAt: firstArmed, Collapsed: collapsed,
		}
		// Max-hold cap, checked on the RE-ARM path because that is the only
		// path a never-quiet branch ever takes. Without this a branch whose
		// median inter-merge gap is below the debounce interval — which v4's
		// measurably is — would re-arm forever and never upgrade at all.
		if maxHold > 0 && now.Sub(firstArmed) >= maxHold {
			return autoUpgradeDebounceDecision{
				Allowed:   true,
				Collapsed: collapsed,
				Reason:    "debounce max hold reached — rolling on a busy branch",
			}
		}
		return autoUpgradeDebounceDecision{State: next, Reason: reason}
	}

	// Same target as the pending one: the branch has been quiet since ArmedAt.
	if waited := now.Sub(prev.ArmedAt); waited < interval {
		// The cap applies here too, so a hive cannot be held past it by any
		// combination of quiet and busy cycles.
		if maxHold > 0 && !prev.FirstArmedAt.IsZero() && now.Sub(prev.FirstArmedAt) >= maxHold {
			return autoUpgradeDebounceDecision{
				Allowed:   true,
				Collapsed: prev.Collapsed,
				Reason:    "debounce max hold reached — rolling before the window elapsed",
			}
		}
		return autoUpgradeDebounceDecision{
			State:  prev,
			Reason: "debounce holding — branch not yet quiet",
		}
	}
	// Window elapsed on a stable target. Roll, and clear the record.
	return autoUpgradeDebounceDecision{
		Allowed:   true,
		Collapsed: prev.Collapsed,
		Reason:    "debounce window elapsed — branch quiet",
	}
}

// persistUpgradeDebounceState writes a hive's pending-target record to its
// meta.json, and mirrors it onto the in-memory copy the caller is iterating so
// the two cannot disagree within a cycle.
//
// The zero state CLEARS the record (all three fields are omitempty), which is
// what the fire path wants. A save failure is logged but never fatal: the cost
// of a lost write is a re-armed window — one extra quiet period before the roll
// — which is strictly better than skipping the upgrade over a disk error.
func (s *HubServer) persistUpgradeDebounceState(h *SaaSHive, st autoUpgradeDebounceState) {
	if h == nil {
		return
	}
	// Mutate the caller's copy first so the rest of THIS cycle sees the new
	// state even if the disk write below fails.
	h.AutoUpgradePendingTarget = st.Target
	h.AutoUpgradePendingSince = st.ArmedAt
	h.AutoUpgradePendingFirst = st.FirstArmedAt
	h.AutoUpgradeCollapsed = st.Collapsed

	// Re-read from disk rather than saving the loop's copy wholesale: that copy
	// is a snapshot taken at the top of the sweep, and writing it back would
	// clobber any field another handler has changed since. Same pattern the
	// fire-date persistence uses.
	stored := loadSaaSHive(h.ID)
	if stored == nil {
		stored = h
	}
	stored.AutoUpgradePendingTarget = st.Target
	stored.AutoUpgradePendingSince = st.ArmedAt
	stored.AutoUpgradePendingFirst = st.FirstArmedAt
	stored.AutoUpgradeCollapsed = st.Collapsed
	if err := saveSaaSHive(stored); err != nil {
		s.logger.Warn("failed to persist auto-upgrade debounce state — a hub restart would re-arm the window",
			"hive_id", h.ID, "target", st.Target, "error", err)
	}
}
