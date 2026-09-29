//go:build !windows

package dashboard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func startBlockedSpekExecutor(t *testing.T) (*ContributeWSHub, *Server, *SpekHubExecutor, <-chan struct{}, chan struct{}, string) {
	t.Helper()
	hub, s, _, _ := spekHub(t)
	st := spekHubStage{runKey: spekRunKey, stage: StageSpec, gen: 1}
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}
	s.SetStageExecutor(e)
	work := spekHubRunWorktreePath(e.Identity, st.runKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cleanup registration order ensures no worker touches the fixture after it
	// resets the process-wide workspace and lifecycle store.
	t.Cleanup(e.Stop)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	e.Tick(context.Background(), time.Now())
	waitSpekSignal(t, started, "workspace preparation")
	return hub, s, e, cancelled, release, work
}

func waitSpekSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestSpekHubExecutorRevocationPreservesWorkUntilJoined(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "removed"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			hub, _, e, cancelled, release, work := startBlockedSpekExecutor(t)
			hub.leaseMu.Lock()
			for key, lease := range hub.leases {
				if reset {
					lease.gen++
				} else {
					delete(hub.leases, key)
				}
			}
			hub.leaseMu.Unlock()
			if err := e.sweepStaleWorktrees(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitSpekSignal(t, cancelled, "lease cancellation")
			if _, err := os.Stat(work); err != nil {
				t.Fatalf("swept running worktree: %v", err)
			}
			if !e.IsExecuting(spekRunKey, StageSpec) {
				t.Fatal("released slot before worker exited")
			}
			close(release)
			e.Stop()
			if e.Status().Running != 0 {
				t.Fatal("worker survived Stop")
			}
			if e.Status().LastError != "" {
				t.Fatalf("revocation counted as failure: %s", e.Status().LastError)
			}
			hub.leaseMu.Lock()
			clear(hub.leases)
			hub.leaseMu.Unlock()
			e.mu.Lock()
			e.held["obsolete"] = true
			e.mu.Unlock()
			if err := e.sweepStaleWorktrees(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale worktree retained: %v", err)
			}
			if len(e.held) != 0 || len(e.activity) != 0 {
				t.Fatal("obsolete bookkeeping retained")
			}
			e.Tick(context.Background(), time.Now())
			if e.Status().Running != 0 {
				t.Fatal("stopped executor launched work")
			}
		})
	}
}

func TestSpekHubExecutorShutdownAndReplacementJoinWorkers(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "shutdown"
		if replace {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			hub, s, e, cancelled, release, _ := startBlockedSpekExecutor(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if replace {
					s.SetStageExecutor(nil)
				} else {
					s.Close()
				}
			}()
			waitSpekSignal(t, cancelled, "shutdown cancellation")
			select {
			case <-done:
				t.Fatal("shutdown returned before worker joined")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			waitSpekSignal(t, done, "shutdown join")
			e.Tick(context.Background(), time.Now())
			if e.Status().Running != 0 {
				t.Fatal("stopped executor relaunched persisted lease")
			}
			// A generation cancelled by shutdown was not spent: it must not be
			// recorded as a failure, and its lease keeps its generation so the
			// next hub relaunches it rather than a retry.
			if msg := e.Status().LastError; msg != "" {
				t.Fatalf("shutdown counted as failure: %s", msg)
			}
			assertSpekLeaseGens(t, hub, 1)
		})
	}
}

func assertSpekLeaseGens(t *testing.T, hub *ContributeWSHub, want uint64) {
	t.Helper()
	hub.leaseMu.Lock()
	defer hub.leaseMu.Unlock()
	if len(hub.leases) == 0 {
		t.Fatal("stage lease vanished")
	}
	for key, lease := range hub.leases {
		if lease.gen != want {
			t.Fatalf("lease %s at gen %d, want %d", key, lease.gen, want)
		}
	}
}

// A launch the fence refuses (another process still owns the run's worktree)
// neither fails nor spends the generation: once the owner releases the lock the
// next tick launches that same generation.
func TestSpekHubExecutorFencedLaunchRetriesSameGeneration(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	started, release := make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}
	s.SetStageExecutor(e)
	work := spekHubRunWorktreePath(e.Identity, spekRunKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	t.Cleanup(func() { close(release) })
	orphan, err := acquireSpekHubFence(filepath.Join(filepath.Dir(work), ".executor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()

	e.Tick(context.Background(), time.Now())
	e.mu.Lock()
	var worker <-chan struct{}
	for _, run := range e.inFlight {
		worker = run.done
	}
	e.mu.Unlock()
	if worker != nil {
		waitSpekSignal(t, worker, "fenced worker exit")
	}
	select {
	case <-started:
		t.Fatal("launched while another process owned the worktree")
	default:
	}
	if msg := e.Status().LastError; msg != "" {
		t.Fatalf("fenced launch counted as failure: %s", msg)
	}
	assertSpekLeaseGens(t, hub, 1)

	orphan.Close()
	e.Tick(context.Background(), time.Now())
	waitSpekSignal(t, started, "launch after the fence was released")
}

func TestSpekHubExecutorStatusCaptureStopJoinsPoll(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}
	_, stop := e.startStageStatusCapture(context.Background(), spekHubStage{}, t.TempDir(), nil, StageSpec, "test")
	waitSpekSignal(t, started, "status poll")
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	waitSpekSignal(t, cancelled, "poll cancellation")
	select {
	case <-done:
		t.Error("stop returned before status poll exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	waitSpekSignal(t, done, "poll join")
	stop() // idempotent
}

func TestSpekHubExecutorFenceSurvivesParentClose(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	work := spekHubRunWorktreePath(e.Identity, spekRunKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(work), ".executor.lock")
	fence, err := acquireSpekHubFence(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	spekHubInheritFence(cmd, context.WithValue(ctx, spekHubFenceContextKey{}, fence))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	// Simulate the old hub disappearing while the CLI retains the descriptor.
	fence.Close()
	if other, err := acquireSpekHubFence(path); !errors.Is(err, errSpekHubFenceBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second hub acquired orphan's lock: %v", err)
	}
	if err := e.executeStage(context.Background(), spekHubStage{runKey: spekRunKey}); !errors.Is(err, errSpekHubFenceBusy) {
		t.Fatalf("double launch not fenced: %v", err)
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("swept orphan's worktree: %v", err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireSpekHubFence(path)
	if err != nil {
		t.Fatalf("lock not released on exit: %v", err)
	}
	next.Close()
}

func TestSpekHubExecutorOutputTailIsBounded(t *testing.T) {
	var w spekHubTailWriter
	full := strings.Repeat("a", spekHubOutputTailBytes*3) + strings.Repeat("b", 1024)
	for i := 0; i < len(full); i += 1000 {
		end := min(i+1000, len(full))
		n, err := w.Write([]byte(full[i:end]))
		if n != end-i || err != nil {
			t.Fatal("short write", n, err)
		}
		if len(w.tail) > spekHubOutputTailBytes {
			t.Fatal("unbounded output")
		}
	}
	if w.String() != full[len(full)-spekHubOutputTailBytes:] {
		t.Fatal("wrong diagnostic tail")
	}
	_, _ = w.Write([]byte(full))
	if w.String() != full[len(full)-spekHubOutputTailBytes:] {
		t.Fatal("wrong oversized-write tail")
	}
}

func TestSpekHubExecutorCommandStreamsFullLogWithBoundedTail(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	work := t.TempDir()
	out, _, err := e.runStageCommand(context.Background(), work, os.Environ(), spekHubStage{stage: StageSpec, gen: 1}, []string{"sh", "-c", `head -c 131072 /dev/zero | tr '\000' x; printf '\nEND\n'`})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != spekHubOutputTailBytes || !strings.HasSuffix(string(out), "\nEND\n") {
		t.Fatalf("unexpected tail length=%d", len(out))
	}
	log, err := os.ReadFile(filepath.Join(work, ".hive", "spek-stage-spec-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 131072+5 {
		t.Fatalf("full log truncated: %d bytes", len(log))
	}
}
