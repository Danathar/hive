package testutil

import (
	"fmt"
	"os"
	"os/exec"
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
// Doing that in-process is not possible: a failing subtest marks every ancestor
// failed, so t.Run's return value cannot be used to "absorb" the failure — the
// parent goes red too. (That is itself worth stating, because the obvious
// in-process version of this test LOOKS correct and turns the suite red.)
//
// So the subject runs in a SUBPROCESS: TestGuardSubject below is a helper that
// only executes when HIVE_TESTUTIL_SUBPROCESS is set, and the tests re-exec the
// test binary to run it, then inspect the child's exit status and output. The
// child's failure is then data, not a verdict on this process.

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
