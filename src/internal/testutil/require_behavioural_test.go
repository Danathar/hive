package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// These tests assert the PROPERTY that matters — that the flag flips a skip
// into a real test failure — rather than the shape of the code. The #5388
// family of defects is precisely a guard that reports PASS against the bug it
// exists to catch (the shell half's first version denied the wrong gate and
// printed a false PASS in CI), so the observation here has to be the actual
// outcome of a real *testing.T.
//
// Doing that in-process against a REAL *testing.T is not possible: a failing
// subtest marks every ancestor failed, so t.Run's return value cannot be used
// to "absorb" the failure — the parent goes red too. (That is itself worth
// stating, because the obvious in-process version of this test LOOKS correct
// and turns the suite red.)
//
// So the real-T half runs in a SUBPROCESS: TestGuardSubject below is a helper
// that only executes when HIVE_TESTUTIL_SUBPROCESS is set, and the tests
// re-exec the test binary to run it, then inspect the child's exit status and
// output. The child's failure is then data, not a verdict on this process.
//
// The subprocess proves the guard against the genuine article, but the child's
// coverage counters die with the child, so the guard measures 0% and trips the
// coverage gate (#5597). The *_InProcess tests further down therefore exercise
// the same contract against guardTB — a testing.TB double whose Fatalf/Skip
// end the goroutine exactly as the real methods do — in this process, where
// the profile can see it. Both halves stay: the double for the counters and
// the fine-grained observations, the subprocess for proof against a real T.

const subprocessEnv = "HIVE_TESTUTIL_SUBPROCESS"

// TestGuardSubject is the subject under observation, not a test of its own. It
// no-ops unless re-exec'd by runSubject, so a normal `go test ./...` neither
// runs nor reports it.
func TestGuardSubject(t *testing.T) {
	mode := os.Getenv(subprocessEnv)
	if mode == "" {
		t.Skip("helper process: only runs when re-exec'd by runSubject")
	}
	switch mode {
	case "skip":
		SkipUnlessRequired(t, "planted precondition")
	case "skipf":
		SkipfUnlessRequired(t, "cannot write %s: %v", "config.json", errStub{})
	default:
		t.Fatalf("unknown subject mode %q", mode)
	}
	// Reached only if the guard RETURNED — which neither branch may do.
	t.Error("guard returned instead of skipping or failing")
}

// runSubject re-executes this test binary to run TestGuardSubject with the
// given guard mode and HIVE_TEST_REQUIRE_BEHAVIOURAL value, returning whether
// the child passed and everything it printed.
func runSubject(t *testing.T, mode, flag string) (passed bool, output string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardSubject$", "-test.v")
	cmd.Env = append(os.Environ(), subprocessEnv+"="+mode)
	if flag == "" {
		// Explicitly clear it: the parent lane may have it set, and "inherited
		// from the environment" is exactly the ambiguity this test must not have.
		cmd.Env = append(cmd.Env, RequireBehaviouralEnv+"=")
	} else {
		cmd.Env = append(cmd.Env, RequireBehaviouralEnv+"="+flag)
	}

	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func TestSkipUnlessRequired_FailsUnderFlag(t *testing.T) {
	passed, out := runSubject(t, "skip", "1")

	if passed {
		t.Fatalf("subject PASSED under %s=1; the guard did not make the skip fatal.\n%s",
			RequireBehaviouralEnv, out)
	}
	// Passing is the only way to be wrong in the "did it fail" sense, but a
	// failure for the WRONG reason (a panic, a build error, the "guard
	// returned" line) would also be non-zero. Pin the reason.
	if !strings.Contains(out, "BROKEN TEST") {
		t.Errorf("subject failed, but not via the guard's diagnosis:\n%s", out)
	}
	if strings.Contains(out, "guard returned instead") {
		t.Errorf("guard returned normally under the flag:\n%s", out)
	}
	if strings.Contains(out, "--- SKIP") {
		t.Errorf("subject SKIPPED under %s=1:\n%s", RequireBehaviouralEnv, out)
	}
}

func TestSkipUnlessRequired_SkipsWithoutFlag(t *testing.T) {
	passed, out := runSubject(t, "skip", "")

	if !passed {
		t.Fatalf("subject failed with %s unset; the default must stay permissive.\n%s",
			RequireBehaviouralEnv, out)
	}
	// A passing child is not enough: a guard that simply RETURNED would also
	// pass. Require that it actually skipped.
	if !strings.Contains(out, "--- SKIP") {
		t.Errorf("subject passed but did not skip:\n%s", out)
	}
	if strings.Contains(out, "guard returned instead") {
		t.Errorf("guard returned normally instead of skipping:\n%s", out)
	}
}

// Only the exact value "1" may arm the guard, matching hive_test_skip's
// `[ "$HIVE_TEST_REQUIRE_BEHAVIOURAL" = "1" ]`. A lane exporting "0" or "true"
// must not silently acquire fatal skips.
func TestSkipUnlessRequired_OnlyExactlyOneArms(t *testing.T) {
	for _, v := range []string{"0", "true", "yes", "2", " 1", "01"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(RequireBehaviouralEnv, v)
			if RequireBehavioural() {
				t.Fatalf("%s=%q armed the guard; only \"1\" may", RequireBehaviouralEnv, v)
			}

			passed, out := runSubject(t, "skip", v)
			if !passed {
				t.Fatalf("%s=%q made the skip fatal:\n%s", RequireBehaviouralEnv, v, out)
			}
			if !strings.Contains(out, "--- SKIP") {
				t.Errorf("%s=%q: subject did not skip:\n%s", RequireBehaviouralEnv, v, out)
			}
		})
	}
}

func TestSkipfUnlessRequired_FormatsAndFails(t *testing.T) {
	passed, out := runSubject(t, "skipf", "1")

	if passed {
		t.Fatalf("subject PASSED under %s=1:\n%s", RequireBehaviouralEnv, out)
	}
	// The formatted reason must survive into the failure message, not be
	// replaced by the raw format string.
	if !strings.Contains(out, "cannot write config.json: permission denied") {
		t.Errorf("formatted reason missing from failure output:\n%s", out)
	}
	if strings.Contains(out, "%s") || strings.Contains(out, "%v") {
		t.Errorf("format verbs leaked unrendered into the failure output:\n%s", out)
	}
}

// The rendered message must carry the caller's reason, name the flag, and give
// the diagnosis, so a CI log reader knows the fix is to repair the test or
// unset the flag for that lane — never to add another skip.
func TestFailureMessageNamesFlagAndDiagnosis(t *testing.T) {
	msg := fmt.Sprintf(failureTemplate, "cannot create tmux session", RequireBehaviouralEnv)

	for _, want := range []string{
		"cannot create tmux session", // the caller's reason survives
		RequireBehaviouralEnv + "=1", // which flag armed this
		"BROKEN TEST",                // the diagnosis
		"not an unsuitable environment",
		"#5388",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message does not mention %q:\n%s", want, msg)
		}
	}
}

type errStub struct{}

func (errStub) Error() string { return "permission denied" }

// ── In-process tests against a testing.TB double ────────────────────────────

// guardTB is a testing.TB double for the guards. Unlike recordingTB (whose
// Fatalf must RETURN so Eventually's deadline test can keep asserting),
// guardTB's Fatalf/Skip/Skipf end the calling goroutine via runtime.Goexit,
// exactly as the real *testing.T methods do — the guards' documented contract
// is that they do not return, and a double that returned would let the code
// after the guard run and hide a missing-termination bug (the "guard returned"
// defect the subprocess tests also watch for). Embedding testing.TB satisfies
// the interface's unexported method; any method the guard newly grows a
// dependency on panics on the nil embed, which is the desired signal.
type guardTB struct {
	testing.TB
	helperCalls int
	fataled     bool
	fatal       string
	skipped     bool
	skip        string
}

func (g *guardTB) Helper() { g.helperCalls++ }

func (g *guardTB) Fatalf(format string, args ...any) {
	g.fataled = true
	g.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (g *guardTB) Skip(args ...any) {
	g.skipped = true
	g.skip = fmt.Sprint(args...)
	runtime.Goexit()
}

func (g *guardTB) Skipf(format string, args ...any) {
	g.skipped = true
	g.skip = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// callGuard invokes fn against a fresh guardTB on a dedicated goroutine —
// necessary because the guard is expected to END that goroutine — and reports
// whether fn returned normally instead, which is always a bug in the guard.
func callGuard(fn func(tb testing.TB)) (tb *guardTB, returned bool) {
	tb = &guardTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
		returned = true
	}()
	<-done
	return tb, returned
}

func TestSkipUnlessRequired_InProcess_SkipsByDefault(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "")

	tb, returned := callGuard(func(tb testing.TB) {
		SkipUnlessRequired(tb, "no tmux on this runner")
	})

	if returned {
		t.Fatal("guard returned; like t.Skip it must end the calling goroutine")
	}
	if tb.fataled {
		t.Errorf("guard failed the test with the flag unset:\n%s", tb.fatal)
	}
	if !tb.skipped {
		t.Fatal("guard neither skipped nor failed")
	}
	if tb.skip != "no tmux on this runner" {
		t.Errorf("skip reason = %q, want the caller's reason verbatim", tb.skip)
	}
	if tb.helperCalls == 0 {
		t.Error("guard did not call t.Helper(); failures would point at the guard, not the caller")
	}
}

func TestSkipUnlessRequired_InProcess_FailsWhenArmed(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "1")

	tb, returned := callGuard(func(tb testing.TB) {
		SkipUnlessRequired(tb, "planted precondition")
	})

	if returned {
		t.Fatal("guard returned; like t.Fatal it must end the calling goroutine")
	}
	if tb.skipped {
		t.Errorf("guard skipped under %s=1: %q", RequireBehaviouralEnv, tb.skip)
	}
	if !tb.fataled {
		t.Fatal("guard did not fail the test under the flag")
	}
	if tb.helperCalls == 0 {
		t.Error("guard did not call t.Helper()")
	}
	for _, want := range []string{
		"planted precondition",       // the caller's reason survives
		RequireBehaviouralEnv + "=1", // which flag armed this
		"BROKEN TEST",                // the diagnosis
	} {
		if !strings.Contains(tb.fatal, want) {
			t.Errorf("failure message does not mention %q:\n%s", want, tb.fatal)
		}
	}
}

func TestSkipfUnlessRequired_InProcess_SkipsFormattedByDefault(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "")

	tb, returned := callGuard(func(tb testing.TB) {
		SkipfUnlessRequired(tb, "cannot write %s: %v", "config.json", errStub{})
	})

	if returned {
		t.Fatal("guard returned; like t.Skipf it must end the calling goroutine")
	}
	if tb.fataled {
		t.Errorf("guard failed the test with the flag unset:\n%s", tb.fatal)
	}
	if !tb.skipped {
		t.Fatal("guard neither skipped nor failed")
	}
	if want := "cannot write config.json: permission denied"; tb.skip != want {
		t.Errorf("skip reason = %q, want %q (format must be rendered, not passed raw)", tb.skip, want)
	}
}

func TestSkipfUnlessRequired_InProcess_FailsFormattedWhenArmed(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "1")

	tb, returned := callGuard(func(tb testing.TB) {
		SkipfUnlessRequired(tb, "cannot write %s: %v", "config.json", errStub{})
	})

	if returned {
		t.Fatal("guard returned; like t.Fatal it must end the calling goroutine")
	}
	if tb.skipped {
		t.Errorf("guard skipped under %s=1: %q", RequireBehaviouralEnv, tb.skip)
	}
	if !tb.fataled {
		t.Fatal("guard did not fail the test under the flag")
	}
	if !strings.Contains(tb.fatal, "cannot write config.json: permission denied") {
		t.Errorf("formatted reason missing from failure message:\n%s", tb.fatal)
	}
	if strings.Contains(tb.fatal, "%s") || strings.Contains(tb.fatal, "%v") {
		t.Errorf("format verbs leaked unrendered into the failure message:\n%s", tb.fatal)
	}
	if !strings.Contains(tb.fatal, "BROKEN TEST") {
		t.Errorf("failure message lacks the diagnosis:\n%s", tb.fatal)
	}
}
