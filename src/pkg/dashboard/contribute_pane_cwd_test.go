package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A contributor's agy backend died ~30s after every task delivery, exiting 2,
// with no crash, no log and no signal. The cause was not agy, the relay or the
// prompt: the tmux SERVER had been running since a Go test created it hours
// earlier, holding a working directory inside a nested clone — v2/pkg/agent,
// orphaned when the repo renamed v2/ -> src/. Every pane that server forked
// inherited the deleted cwd ("shell-init: error retrieving current directory"),
// and agy refuses to run without a resolvable one. claude/codex/goose tolerate
// it, which is why it looked backend-specific.
//
// `tmux new-session -c <valid path>` does NOT rescue this: on a server whose own
// cwd is gone, the pane is still forked into the deleted directory. Only a cd in
// the launch command itself is reliable, so these tests pin the cd — reading the
// Justfile rather than restating it, so the recipe cannot regress quietly.

func justfileSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "Justfile")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// contributeHiveLaunchBlock returns the lines of contribute-hive that create the
// tmux session and type the CLI launch command into it.
func contributeHiveLaunchBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, "Create tmux session with the CLI")
	if start < 0 {
		t.Fatal("tmux session creation block not found in the Justfile")
	}
	end := strings.Index(src[start:], "# Start the relay")
	if end < 0 {
		t.Fatal("end of the session creation block not found in the Justfile")
	}
	return src[start : start+end]
}

func TestContributeHivePinsPaneWorkingDirectory(t *testing.T) {
	block := contributeHiveLaunchBlock(t)

	// The load-bearing half: the launch command itself must cd first, so the CLI
	// starts in a real directory whatever cwd the tmux server hands the pane.
	sendKeys := ""
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "send-keys") && strings.Contains(line, "$CMD") {
			sendKeys = line
			break
		}
	}
	if sendKeys == "" {
		t.Fatal("no send-keys line launching $CMD found in the block")
	}
	if !strings.Contains(sendKeys, "cd ") {
		t.Errorf("the CLI launch must cd into the repo first — a tmux server can hand the pane a DELETED cwd, "+
			"and a backend that needs a resolvable one then dies seconds into its first task: %s",
			strings.TrimSpace(sendKeys))
	}

	// Defense in depth, correct on a healthy server (but not sufficient alone).
	if !strings.Contains(block, "new-session") || !strings.Contains(block, "-c ") {
		t.Error("new-session should also pass -c so a healthy server starts the pane in the right directory")
	}

	// A poisoned server must be reported, not silently worked around: without a
	// message, the next person debugs an unexplained backend exit for hours.
	if !strings.Contains(block, "pane_current_path") {
		t.Error("the recipe must check the pane's working directory and warn when it is gone")
	}
	if !strings.Contains(block, "tmux kill-server") {
		t.Error("the warning must tell the contributor how to fix a poisoned tmux server")
	}
}
