package hub

// hub_testing_helpers_invariant_test.go pins what newHubServerForTest promises,
// so a refactor of the helper cannot quietly hand the suite back the shared
// /data footprint it was written to remove.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hubTestRun records what one helper-built server saw, for inspection AFTER
// that subtest's cleanups have run.
type hubTestRun struct {
	dir      string
	registry string
	secret   string
	srv      *HubServer
}

// TestNewHubServerForTest_IsolatesEachTest proves the three invariants:
//
//  1. consecutive tests get DIFFERENT registry and secret paths, each under
//     its own TempDir, and the package vars are restored between them;
//  2. the generated master secret lands at hubSecretPath under that TempDir —
//     never under /data — and is the secret the server actually holds;
//  3. once the test's cleanup has run, its save loop has exited (saveLoopDone
//     is closed) and its TempDir is gone, i.e. the loop was joined BEFORE the
//     directory was removed rather than left to race it (#4774).
func TestNewHubServerForTest_IsolatesEachTest(t *testing.T) {
	prodRegistry, prodSecret := registryPath, hubSecretPath
	// Snapshot every managed var rather than assuming the production default:
	// an earlier test in the process may legitimately have left a read-only
	// var moved, and the helper's contract is "restore what it found", not
	// "restore production".
	before := make([]string, len(hubTestPathVars))
	for i, v := range hubTestPathVars {
		before[i] = *v.ptr
	}

	var runs []hubTestRun
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			// Force the file path: with the env var set NewHubServer would
			// never touch hubSecretPath and the secret assertions below would
			// pass vacuously.
			t.Setenv("HIVE_HUB_SECRET", "")

			srv := newHubServerForTest(t)
			run := hubTestRun{
				dir:      filepath.Dir(registryPath),
				registry: registryPath,
				secret:   hubSecretPath,
				srv:      srv,
			}
			runs = append(runs, run)

			if srv.registryPath != registryPath {
				t.Fatalf("server captured registryPath %q but the helper set %q", srv.registryPath, registryPath)
			}
			if strings.HasPrefix(registryPath, "/data") || strings.HasPrefix(hubSecretPath, "/data") {
				t.Fatalf("helper left a production path in place: registry=%q secret=%q", registryPath, hubSecretPath)
			}
			if !strings.HasPrefix(hubSecretPath, run.dir+string(os.PathSeparator)) {
				t.Fatalf("secret %q is not under the per-test dir %q", hubSecretPath, run.dir)
			}
			data, err := os.ReadFile(hubSecretPath)
			if err != nil {
				t.Fatalf("generated secret was not written under the TempDir: %v", err)
			}
			if got := strings.TrimSpace(string(data)); got != srv.hubSecret {
				t.Fatalf("secret on disk %q != secret the server holds %q", got, srv.hubSecret)
			}
		})
	}

	if registryPath != prodRegistry || hubSecretPath != prodSecret {
		t.Fatalf("package path vars not restored after cleanup: registry=%q secret=%q", registryPath, hubSecretPath)
	}
	for i, v := range hubTestPathVars {
		if *v.ptr != before[i] {
			t.Errorf("path var for %q not restored: %q != %q", v.rel, *v.ptr, before[i])
		}
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].registry == runs[1].registry || runs[0].secret == runs[1].secret || runs[0].dir == runs[1].dir {
		t.Fatalf("consecutive tests shared a path: %+v vs %+v", runs[0], runs[1])
	}
	for _, run := range runs {
		select {
		case <-run.srv.saveLoopDone:
		default:
			t.Errorf("save loop for %q is still running after the test's cleanup — it was not joined", run.dir)
		}
		if _, err := os.Stat(run.dir); !os.IsNotExist(err) {
			t.Errorf("per-test dir %q still exists after cleanup (stat err=%v)", run.dir, err)
		}
	}
}

// TestNewHubServerForTest_RespectsCallerPaths pins the two ways a test keeps
// control of a path: withRegistryPath for the registry, and moving a read-only
// var off its production default BEFORE calling the helper for the rest.
func TestNewHubServerForTest_RespectsCallerPaths(t *testing.T) {
	own := t.TempDir()
	pinnedRegistry := filepath.Join(own, "pinned-registry.json")
	seededClusters := filepath.Join(own, "clusters.json")

	oldClusters := clustersConfigPath
	clustersConfigPath = seededClusters
	t.Cleanup(func() { clustersConfigPath = oldClusters })

	srv := newHubServerForTest(t, withRegistryPath(pinnedRegistry))
	if srv.registryPath != pinnedRegistry || registryPath != pinnedRegistry {
		t.Fatalf("withRegistryPath not honoured: server=%q var=%q", srv.registryPath, registryPath)
	}
	if clustersConfigPath != seededClusters {
		t.Fatalf("helper overrode a var the test had already moved: %q", clustersConfigPath)
	}
	// The secret is a WRITE target, so it must still be per-test even though
	// the registry was pinned elsewhere.
	if filepath.Dir(filepath.Dir(hubSecretPath)) == own {
		t.Fatalf("secret %q landed in the caller's dir instead of the helper's", hubSecretPath)
	}
}
