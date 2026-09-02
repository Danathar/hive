package dashboard

import "testing"

// restoreHealthCaches is the isolation hook for the package-level health
// caches (#5570). buildHealth's live-client path writes cachedHealth and can
// refresh cachedGreenStreak (status_builder.go), and nothing in non-test code
// ever clears either — so any test that reaches that path, or seeds the caches
// directly as a fixture, leaks state into every later test that exercises the
// nil-client branch and asserts the {"ci": 100} default. Under -shuffle that
// surfaces as an order-dependent failure (e.g. TestBuildHealth_NilClient_Boost
// failing with "ci = 0" whenever TestCovH2_BuildHealthAndRateLimits happens to
// run first; minimal reproduction: -shuffle=11 with just that pair).
//
// Call this FIRST in any test that writes the caches — directly or via a
// non-nil GitHub client — instead of hand-rolling a snapshot/restore at each
// site. It snapshots both caches and registers a t.Cleanup restoring them, so
// the restore runs even when the test later calls t.Fatalf, and a future
// writer test cannot forget half the pair.
func restoreHealthCaches(tb testing.TB) {
	tb.Helper()

	cachedHealthMu.Lock()
	prevHealth := cachedHealth
	cachedHealthMu.Unlock()

	cachedGreenStreakMu.Lock()
	prevStreak, prevStreakOK := cachedGreenStreak, cachedGreenStreakOK
	cachedGreenStreakMu.Unlock()

	tb.Cleanup(func() {
		cachedHealthMu.Lock()
		cachedHealth = prevHealth
		cachedHealthMu.Unlock()

		cachedGreenStreakMu.Lock()
		cachedGreenStreak, cachedGreenStreakOK = prevStreak, prevStreakOK
		cachedGreenStreakMu.Unlock()
	})
}

// setCachedHealth seeds the cachedHealth fixture for tests that assert the
// cached-read branch of buildHealth. It routes through restoreHealthCaches so
// seeding a fixture can never leak past the test that set it.
func setCachedHealth(tb testing.TB, health map[string]any) {
	tb.Helper()
	restoreHealthCaches(tb)
	cachedHealthMu.Lock()
	cachedHealth = health
	cachedHealthMu.Unlock()
}
