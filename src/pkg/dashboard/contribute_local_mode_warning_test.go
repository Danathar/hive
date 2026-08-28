package dashboard

import (
	"strings"
	"testing"
)

// #4918: `just contribute-hive <backend> local` runs the backend CLI as the
// contributor's own user, on their own machine, with permission gating
// bypassed and nothing scoping its filesystem access to the workspace. The
// recipe previously described that mode only as "native mode" and said nothing
// about the difference.
//
// What the silence cost: an agent doing entirely correct work on an assigned
// third-party repo ran that repo's own test suite; a latent defect in two of
// its tests let a hook escape its stubs and call `rpm-ostree kargs` against the
// operator's REAL deployment, raising three polkit dialogs on their desktop.
// Nothing was written, and the only reason is that the process happened to lack
// privilege. No compromise was involved.
//
// The remedy on this path is container mode, which is already the default. The
// warning exists so the operator choosing `local` is choosing it knowingly.

// contributeHiveLocalBranch returns the head of contribute-hive's local-mode
// branch, up to the tmux session name it derives. Read from the Justfile rather
// than restated, so the warning cannot be dropped quietly.
func contributeHiveLocalBranch(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, `if [[ "$_MODE" == "local" ]]; then`)
	if start < 0 {
		t.Fatal("contribute-hive local-mode branch not found in the Justfile")
	}
	end := strings.Index(src[start:], "TMUX_SESSION=")
	if end < 0 {
		t.Fatal("end of the local-mode preamble not found in the Justfile")
	}
	return src[start : start+end]
}

// TestLocalModeWarnsItIsUnconfined pins the warning itself.
func TestLocalModeWarnsItIsUnconfined(t *testing.T) {
	block := contributeHiveLocalBranch(t)

	if !strings.Contains(block, "LOCAL MODE") || !strings.Contains(block, "NOT confined") {
		t.Error("contribute-hive local mode no longer states that the agent is unconfined (#4918)")
	}
	// The warning has to name the way out, or it is an alarm rather than
	// guidance. Container mode is the default and is the remedy on this path.
	if !strings.Contains(block, "just contribute-hive ${BACKEND}") {
		t.Error("the local-mode warning does not point at container mode as the confined alternative")
	}
}

// TestLocalModeWarningIsHonestAboutWhatStillHolds. A warning that implies
// NOTHING is constrained is its own kind of wrong: the #4938 host-state denials
// do apply here (the recipe sources config/backends.conf), and credentials and
// pushes are constrained on every path. Saying so is what makes the "not
// constrained" half credible.
func TestLocalModeWarningIsHonestAboutWhatStillHolds(t *testing.T) {
	block := contributeHiveLocalBranch(t)

	for _, want := range []string{"Still constrained", "sudo", "rpm-ostree", "GitHub token"} {
		if !strings.Contains(block, want) {
			t.Errorf("the local-mode warning does not mention %q, so it overstates the exposure", want)
		}
	}
}

// TestContainerModeRemainsTheDefault. The whole remedy above is "drop the word
// local", which only works while container mode is what you get by default.
func TestContainerModeRemainsTheDefault(t *testing.T) {
	src := justfileSource(t)
	if !strings.Contains(src, `contribute-hive backend="" mode="docker":`) {
		t.Error("contribute-hive no longer defaults to container mode; the #4918 warning's advice is stale")
	}
}
