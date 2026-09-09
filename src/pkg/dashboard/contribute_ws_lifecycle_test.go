package dashboard

import (
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

func TestContributeWSHub_CloseStopsCleanupLoopPromptly(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)

	select {
	case <-hub.doneCh:
		t.Fatal("doneCh should not be closed immediately after hub construction")
	default:
	}

	start := time.Now()
	hub.Close()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("hub.Close() took %v, expected prompt termination", elapsed)
	}

	select {
	case <-hub.doneCh:
		// Success: cleanupLoop exited and closed doneCh
	default:
		t.Fatal("doneCh was not closed after Close()")
	}
}

func TestContributeWSHub_CloseIdempotentAndConcurrent(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			hub.Close()
		}()
	}
	wg.Wait()

	// Calling Close sequentially after concurrent closes must also succeed immediately
	hub.Close()
	hub.Stop()

	select {
	case <-hub.doneCh:
	default:
		t.Fatal("doneCh should remain closed")
	}
}

func TestContributeWSHub_CloseNilAndUninitialized(t *testing.T) {
	// Nil receiver must not panic
	var nilHub *ContributeWSHub
	nilHub.Close()
	nilHub.Stop()

	// Hub created directly without NewContributeWSHub (e.g. nil stopCh/doneCh) must not panic or block
	uninitHub := &ContributeWSHub{}
	uninitHub.Close()
	uninitHub.Stop()
}

func TestContributeWSHub_DrainForShutdownDoesNotStopCleanupLoop(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)

	// DrainForShutdown should drain connections with close frames without stopping cleanupLoop
	drained := hub.DrainForShutdown()
	if drained != 0 {
		t.Fatalf("DrainForShutdown() = %d, want 0", drained)
	}

	select {
	case <-hub.doneCh:
		t.Fatal("DrainForShutdown must not stop the background cleanupLoop")
	default:
	}

	// Now close the hub explicitly
	hub.Close()
	select {
	case <-hub.doneCh:
	default:
		t.Fatal("hub.Close() must close doneCh")
	}
}

func TestContributeWSHub_NoGoroutineLeak(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()

	const iterations = 5
	for i := 0; i < iterations; i++ {
		hub := NewContributeWSHub(slog.Default(), nil)
		hub.Close()
	}

	// Verify goroutines settle back down promptly. Polled via testutil.Eventually
	// rather than a fixed sleep so the sleep ratchet in internal/testutil is not
	// pushed above its baseline.
	testutil.Eventually(t, 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseGoroutines+1
	}, "goroutines leaked: baseline=%d", baseGoroutines)
}

func TestServer_CloseContributeHub(t *testing.T) {
	// Nil server is safe
	var nilServer *Server
	nilServer.CloseContributeHub()
	nilServer.Close()

	srv := NewServer(0, slog.Default())
	// No hub registered yet: safe no-op
	srv.CloseContributeHub()
	srv.Close()

	srv.registerContributeRoutes()
	if srv.contributeHub == nil {
		t.Fatal("expected contributeHub to be initialized")
	}

	done := srv.contributeHub.doneCh
	srv.CloseContributeHub()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server.CloseContributeHub did not stop cleanupLoop")
	}

	// Idempotent
	srv.CloseContributeHub()
	srv.Close()
}
