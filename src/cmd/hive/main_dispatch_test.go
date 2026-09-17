package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDispatchSubcommandRoutesValidateToConfigCheck(t *testing.T) {
	path := writeCheckConfig(t, nil)

	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{"validate", "-config", path}, &stdout, &stderr)

	if !handled {
		t.Fatal("dispatchSubcommand(validate) handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("dispatchSubcommand(validate) code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("validate did not run the config-check path; stdout: %s", stdout.String())
	}
}

func TestDispatchSubcommandRoutesConfigCheckAlias(t *testing.T) {
	path := writeCheckConfig(t, nil)

	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{"--config-check", "-config", path}, &stdout, &stderr)

	if !handled {
		t.Fatal("dispatchSubcommand(--config-check) handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("dispatchSubcommand(--config-check) code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("--config-check did not run the config-check path; stdout: %s", stdout.String())
	}
}

func TestDashboardDependenciesWireRescanReposFunc(t *testing.T) {
	// dashboardDependencies is a closure inside main() (v4 layout), so the
	// RescanReposFunc wiring is pinned at the source level.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "dashboardDependencies := func()")
	if start < 0 {
		t.Fatal("main.go no longer defines the dashboardDependencies closure")
	}
	if !strings.Contains(body[start:], "return rescanRepos(rescanCtx, cfg, ghClient") {
		t.Fatal("dashboardDependencies no longer wires RescanReposFunc to rescanRepos")
	}
}
