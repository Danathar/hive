package testutil

import (
	"fmt"
	"os"
	"testing"
)

// RequireBehaviouralEnv is the environment variable that turns a permissive
// skip into a failure. It is deliberately the SAME variable the shell suites
// under src/deploy/ honour via hive_test_skip (src/deploy/test_lib.sh), so one
// flag governs skip discipline across both halves of the test estate (#5388).
const RequireBehaviouralEnv = "HIVE_TEST_REQUIRE_BEHAVIOURAL"

// ── The defect this exists to close ─────────────────────────────────────────
//
// A test that skips on a runner which genuinely cannot run it is CORRECT. The
// same test skipping on a runner that CAN run it means the test is broken —
// and it reports green either way. That is #5388: a skip is a result nobody
// acts on, so a test can quietly stop asserting anything and no lane goes red.
//
// ── The contract ────────────────────────────────────────────────────────────
//
//	HIVE_TEST_REQUIRE_BEHAVIOURAL unset / not "1" (default)
//	    SkipUnlessRequired calls t.Skip. The package stays runnable on a
//	    laptop and on a bare runner that lacks the tooling.
//
//	HIVE_TEST_REQUIRE_BEHAVIOURAL=1
//	    SkipUnlessRequired calls t.Fatal. The CALLER — the lane — is asserting
//	    the precondition holds, so a skip cannot mean "unsuitable environment";
//	    it means the precondition moved and the test silently stopped testing.
//
// Setting the flag is a judgement call and belongs to the LANE, not the test.
// Set it only where the runner GUARANTEES the precondition. Setting it where
// the precondition is merely likely converts a correct skip into a false red,
// and a noisy detector gets ignored.
//
// ── Which skips to convert ──────────────────────────────────────────────────
//
// Convert a skip only when the precondition is guaranteed by the test harness
// itself or by the lane's runner image:
//
//   - writes into t.TempDir() or a directory TestMain created and verified;
//   - fixtures TestMain builds unconditionally and os.Exit(1)s on failure;
//   - operations that follow an already-passed capability check (a tmux
//     command failing AFTER tmux was found on PATH is a broken test, not a
//     missing capability).
//
// Do NOT convert a skip that gates on a genuine capability difference between
// runners — no docker, no tmux binary, tmux older than 3.2, an unprivileged
// runner, a missing kernel module. Those skips are the system working. The
// shell half left docker- and tmux-gated skips permissive for exactly this
// reason; keep the two halves consistent.

// failureTemplate is the printf format used when the flag is armed. Its first
// verb is the caller's reason; its second is RequireBehaviouralEnv. It names
// the flag and states the diagnosis (BROKEN TEST, not unsuitable environment)
// so a CI log reader knows the fix is to repair the test or unset the flag for
// that lane — never to add another skip. Mirrors hive_test_skip's wording.
const failureTemplate = "%s\n\t%s=1 — the caller asserts this precondition holds here,\n" +
	"\tso this is a BROKEN TEST, not an unsuitable environment (#5388)."

// RequireBehavioural reports whether the caller has asserted that the
// preconditions for behavioural tests hold in this environment. Tests needing
// finer control than SkipUnlessRequired (for example, choosing between two
// assertions rather than skipping) can branch on it directly.
func RequireBehavioural() bool {
	return os.Getenv(RequireBehaviouralEnv) == "1"
}

// SkipUnlessRequired skips the test with reason, unless
// HIVE_TEST_REQUIRE_BEHAVIOURAL=1, in which case it fails the test instead.
//
// The failure message names the flag and states that this is a broken test
// rather than an unsuitable environment, mirroring hive_test_skip's wording,
// so whoever reads the CI log knows the fix is to repair the test or unset the
// flag for that lane — not to add another skip.
//
// Like t.Skip and t.Fatal, SkipUnlessRequired does not return: it ends the
// calling goroutine's test. It must be called from the test goroutine.
func SkipUnlessRequired(t *testing.T, reason string) {
	t.Helper()
	if RequireBehavioural() {
		t.Fatalf(failureTemplate, reason, RequireBehaviouralEnv)
	}
	t.Skip(reason)
}

// SkipfUnlessRequired is SkipUnlessRequired with printf-style formatting, for
// the common case of embedding the underlying error in the reason.
func SkipfUnlessRequired(t *testing.T, format string, args ...any) {
	t.Helper()
	if RequireBehavioural() {
		t.Fatalf(failureTemplate, fmt.Sprintf(format, args...), RequireBehaviouralEnv)
	}
	t.Skipf(format, args...)
}
