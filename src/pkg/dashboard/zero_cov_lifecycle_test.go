package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/linearagent"
)

// Covers previously 0%-covered exported funcs in pkg/dashboard:
// SetReleaseChannel (api.go), LinearAgentPROpened (api_linear_agent.go),
// Server.sampleMetricsInputs + StartContributeMetrics (contribute_metrics.go).
// The RepoCostCollector.Start/CollectedAt tests moved to
// pkg/dashboard/collect with the collector (kubestellar/hive#5565 slice 2).

// ---- SetReleaseChannel ----

// TestSetReleaseChannel_VersionResponse asserts the channel set via
// SetReleaseChannel shows up as /api/version's "channel" field, and that an
// empty channel omits the field entirely (the documented "" contract for
// branch-tag / SHA-pinned deployments).
func TestSetReleaseChannel_VersionResponse(t *testing.T) {
	origChannel := versionChannel
	t.Cleanup(func() { versionChannel = origChannel })

	s, _ := apiServer(t)

	SetReleaseChannel("stable")
	result := decodeJSON(t, doGet(s, "/api/version"))
	if result["channel"] != "stable" {
		t.Errorf("channel = %v, want %q", result["channel"], "stable")
	}

	SetReleaseChannel("")
	result = decodeJSON(t, doGet(s, "/api/version"))
	if _, present := result["channel"]; present {
		t.Errorf("channel must be omitted when SetReleaseChannel(%q) was called, got %v", "", result["channel"])
	}
}

// TestSetReleaseChannel_DoesNotAffectUpstreamBranch pins the display-only
// contract: the channel badge must not change which branch the self-version
// check compares against.
func TestSetReleaseChannel_DoesNotAffectUpstreamBranch(t *testing.T) {
	origChannel := versionChannel
	origBranch := versionBranch
	t.Cleanup(func() {
		versionChannel = origChannel
		versionBranch = origBranch
	})

	SetGitBranch("v4")
	before := upstreamBranch()
	SetReleaseChannel("edge")
	if got := upstreamBranch(); got != before {
		t.Errorf("upstreamBranch changed from %q to %q after SetReleaseChannel — channel must be display-only", before, got)
	}
}

// TestSetDeploymentImage_VersionResponse exercises the dashboard-facing API
// contract for all delivery shapes called out in #6321. Classification itself
// lives in pkg/imageref; this test proves the spoke wires that shared result to
// /api/version and withholds untrusted refs when provenance is unknown.
func TestSetDeploymentImage_VersionResponse(t *testing.T) {
	origSource, origChannel, origBranch := versionImageSource, versionChannel, versionBranch
	t.Cleanup(func() { versionImageSource, versionChannel, versionBranch = origSource, origChannel, origBranch })
	SetGitBranch("v5")
	SetReleaseChannel("stale-channel") // must never override the current image
	imageRef, calls := "", 0
	SetDeploymentImageSource(func() string { calls++; return imageRef })

	s, _ := apiServer(t)
	tests := []struct {
		name         string
		imageRef     string
		wantTracking string
		wantImageRef bool
		wantChannel  string
	}{
		{name: "initially unavailable", wantTracking: "unknown"},
		{name: "stable channel", imageRef: "ghcr.io/hivecommons/hive:stable", wantTracking: "floating", wantImageRef: true, wantChannel: "stable"},
		{name: "candidate channel", imageRef: "ghcr.io/hivecommons/hive:candidate", wantTracking: "floating", wantImageRef: true, wantChannel: "candidate"},
		{name: "edge channel", imageRef: "ghcr.io/hivecommons/hive:edge", wantTracking: "floating", wantImageRef: true, wantChannel: "edge"},
		{name: "branch latest", imageRef: "ghcr.io/hivecommons/hive:v5-latest", wantTracking: "floating", wantImageRef: true},
		{name: "sha tag", imageRef: "ghcr.io/hivecommons/hive:61c5ad7", wantTracking: "pinned", wantImageRef: true},
		{name: "version tag", imageRef: "ghcr.io/hivecommons/hive:v5.0.0", wantTracking: "pinned", wantImageRef: true},
		{name: "digest", imageRef: "ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64), wantTracking: "pinned", wantImageRef: true},
		{name: "channel with digest pin", imageRef: "ghcr.io/hivecommons/hive:stable@sha256:" + strings.Repeat("a", 64), wantTracking: "pinned", wantImageRef: true, wantChannel: "stable"},
		{name: "unavailable", imageRef: "", wantTracking: "unknown", wantImageRef: false},
		{name: "malformed", imageRef: "ghcr.io/hivecommons/hive:not a tag", wantTracking: "unknown", wantImageRef: false},
		{name: "secret bearing URL", imageRef: "https://user:secret@registry.example/hive:stable", wantTracking: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imageRef = tt.imageRef
			before := calls
			result := decodeJSON(t, doGet(s, "/api/version"))
			if calls != before+1 {
				t.Fatalf("image lookups = %d, want exactly one per response", calls-before)
			}
			if got := result["channel"]; (tt.wantChannel == "" && got != nil) || (tt.wantChannel != "" && got != tt.wantChannel) {
				t.Errorf("channel = %v, want %q", got, tt.wantChannel)
			}
			if result["branch"] != "v5" || upstreamBranch() != "v5" {
				t.Error("image provenance changed the build/compare branch")
			}
			if result["tracking"] != tt.wantTracking {
				t.Errorf("tracking = %v, want %q", result["tracking"], tt.wantTracking)
			}
			gotRef, present := result["imageRef"]
			if present != tt.wantImageRef {
				t.Fatalf("imageRef present = %v, want %v (value %v)", present, tt.wantImageRef, gotRef)
			}
			if present && gotRef != tt.imageRef {
				t.Errorf("imageRef = %v, want %q", gotRef, tt.imageRef)
			}
		})
	}
}

// ---- LinearAgentPROpened ----

// prOpenedServer builds a Server whose lazy linearAgent() returns the given
// pre-built service, so the test never touches linearagent.DefaultStorePath()
// on disk (hermetic: no /data reads).
func prOpenedServer(t *testing.T, svc *testLinearService) *Server {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.linearAgentSvc = svc
	return s
}

// TestLinearAgentPROpened_NilResponder asserts the hook is a safe no-op when
// the Linear agent service exists but has no responder (store-open failure
// path) — the pr-request watcher calls this unconditionally on every PR.
func TestLinearAgentPROpened_NilResponder(t *testing.T) {
	s := prOpenedServer(t, &testLinearService{})
	// Must not panic.
	s.LinearAgentPROpened("quality", "org/repo", 42, "https://example.test/pr/42")
}

// TestLinearAgentPROpened_NoActiveSession asserts the hook delegates to the
// responder and returns cleanly when the agent has no active Linear session
// (the overwhelmingly common case: most PRs are not Linear-delegated work).
func TestLinearAgentPROpened_NoActiveSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := linearagent.NewResponder(nil, nil, nil, linearagent.NewTracker(), logger)
	s := prOpenedServer(t, &testLinearService{responder: responder})
	// Empty tracker -> HandlePROpened's ActiveSessionForAgent miss -> no-op.
	s.LinearAgentPROpened("quality", "org/repo", 7, "https://example.test/pr/7")
}

// ---- Server.sampleMetricsInputs ----

// TestSampleMetricsInputs_NoHub asserts the sampler is safe with no
// contribute hub wired (queue/fleet stay zero) and reads per-contributor
// cumulative totals from the contributors dir, skipping profiles with no
// GitHub username.
func TestSampleMetricsInputs_NoHub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)

	writeProfile := func(name string, p ContributorProfile) {
		t.Helper()
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write profile: %v", err)
		}
	}
	writeProfile("alice.json", ContributorProfile{
		GitHubUsername: "alice",
		ContributorID:  "c-alice",
		TasksCompleted: 5,
	})
	// No GitHubUsername -> must be skipped by the sampler.
	writeProfile("ghost.json", ContributorProfile{
		ContributorID:  "c-ghost",
		TasksCompleted: 99,
	})

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := time.Now()
	sample := s.sampleMetricsInputs()

	if sample.queueDepth != 0 || sample.fleetSize != 0 {
		t.Errorf("queueDepth/fleetSize = %d/%d, want 0/0 with no contribute hub", sample.queueDepth, sample.fleetSize)
	}
	if got := sample.userTotals["alice"]; got != 5 {
		t.Errorf("userTotals[alice] = %d, want 5", got)
	}
	if len(sample.userTotals) != 1 {
		t.Errorf("userTotals = %v, want only alice (username-less profiles skipped)", sample.userTotals)
	}
	if sample.now.Before(before) {
		t.Errorf("sample.now = %v predates the call", sample.now)
	}
}

// ---- Server.StartContributeMetrics ----

// TestStartContributeMetrics_RollsUpAndStopsOnCancel asserts the startup
// wiring actually drives the hourly rollup loop with the Server as sampler
// (buckets appear) and that cancelling ctx shuts the goroutine down (no leak:
// bucket count stops growing).
func TestStartContributeMetrics_RollsUpAndStopsOnCancel(t *testing.T) {
	t.Setenv("HIVE_METRICS_FILE", filepath.Join(t.TempDir(), "metrics.json"))
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())

	origInterval := metricsRollupInterval
	metricsRollupInterval = 5 * time.Millisecond
	t.Cleanup(func() { metricsRollupInterval = origInterval })

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartContributeMetrics(ctx)

	store := s.contributeMetricsStore()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.queueDepth)
		store.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("StartContributeMetrics never rolled up a bucket")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	// Give the loop time to observe cancellation, then verify it stopped.
	time.Sleep(20 * time.Millisecond)
	store.mu.Lock()
	after := len(store.queueDepth)
	store.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	store.mu.Lock()
	final := len(store.queueDepth)
	store.mu.Unlock()
	if final != after {
		t.Errorf("rollup kept running after ctx cancel: buckets %d -> %d", after, final)
	}
}
