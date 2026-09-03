package testutil

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordingTB is a testing.TB double that records Fatalf instead of aborting
// the goroutine, so the negative case can observe the deadline failure without
// failing the real test. Embedding the interface satisfies testing.TB's
// unexported method; only Helper and Fatalf are reached by Eventually, so the
// nil embedded value is never dereferenced.
type recordingTB struct {
	testing.TB
	helperCalls int
	failed      bool
	fatal       string
}

func (r *recordingTB) Helper() { r.helperCalls++ }

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.fatal = fmt.Sprintf(format, args...)
}

func TestEventually_ReturnsOnceConditionHolds(t *testing.T) {
	const trueOnCall = 3
	var calls atomic.Int32
	start := time.Now()
	Eventually(t, 5*time.Second, func() bool {
		return calls.Add(1) >= trueOnCall
	}, "counter never reached %d", trueOnCall)
	if got := calls.Load(); got != trueOnCall {
		t.Fatalf("cond called %d times, want exactly %d (must stop polling once true)", got, trueOnCall)
	}
	// Two polls' worth of waiting is far below the 5s timeout: Eventually must
	// not sit out its full budget once the condition is satisfied.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Eventually took %s after the condition held; expected ~%s", elapsed, 2*pollInterval)
	}
}

func TestEventually_FailsWithMessageOnDeadline(t *testing.T) {
	rec := &recordingTB{}
	const timeout = 30 * time.Millisecond
	start := time.Now()
	Eventually(rec, timeout, func() bool { return false }, "widget %d never became %s", 42, "ready")

	if !rec.failed {
		t.Fatal("Eventually did not call Fatalf on deadline")
	}
	if rec.helperCalls == 0 {
		t.Error("Eventually did not call t.Helper()")
	}
	for _, want := range []string{"widget 42 never became ready", timeout.String()} {
		if !strings.Contains(rec.fatal, want) {
			t.Errorf("Fatalf message %q does not contain %q", rec.fatal, want)
		}
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("Eventually gave up after %s, before the %s timeout", elapsed, timeout)
	}
}

func TestEventuallyValue_ReturnsObservedValue(t *testing.T) {
	var calls atomic.Int32
	got := EventuallyValue(t, 5*time.Second, func() (string, bool) {
		if calls.Add(1) < 2 {
			return "", false
		}
		return "arrived", true
	}, "value never produced")
	if got != "arrived" {
		t.Fatalf("EventuallyValue = %q, want %q", got, "arrived")
	}
}

func TestEventuallyValue_ZeroValueAndFailureOnDeadline(t *testing.T) {
	rec := &recordingTB{}
	got := EventuallyValue(rec, 20*time.Millisecond, func() (int, bool) { return 7, false }, "no value")
	if !rec.failed {
		t.Fatal("EventuallyValue did not call Fatalf on deadline")
	}
	if got != 0 {
		t.Fatalf("EventuallyValue returned %d on failure, want the zero value", got)
	}
}

func TestEventuallyEveryFunc_ReturnsOnceConditionHolds(t *testing.T) {
	const trueOnCall = 3
	var calls atomic.Int32
	var describeCalls atomic.Int32
	start := time.Now()
	EventuallyEveryFunc(t, 5*time.Second, time.Millisecond, func() bool {
		return calls.Add(1) >= trueOnCall
	}, func() string {
		describeCalls.Add(1)
		return "must never be rendered"
	})
	if got := calls.Load(); got != trueOnCall {
		t.Fatalf("cond called %d times, want exactly %d (must stop polling once true)", got, trueOnCall)
	}
	// describe's contract is "at most once, only on the failure path": a
	// describe that renders frames or dumps queues must cost nothing when the
	// wait succeeds.
	if got := describeCalls.Load(); got != 0 {
		t.Fatalf("describe called %d times on the success path, want 0", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("EventuallyEveryFunc took %s after the condition held; expected a few polls' worth", elapsed)
	}
}

// The function's entire reason to exist: the failure message must describe the
// system AT THE DEADLINE, not at t=0. cond counts its own polls and describe
// reports the count; an implementation that captured the description eagerly
// (at entry, before the first poll — exactly what the printf-argument form it
// replaces did) would report 0.
func TestEventuallyEveryFunc_DescribeIsLazyAndAtMostOnce(t *testing.T) {
	rec := &recordingTB{}
	var polls atomic.Int32
	var describeCalls atomic.Int32
	const timeout = 30 * time.Millisecond
	start := time.Now()
	EventuallyEveryFunc(rec, timeout, 5*time.Millisecond, func() bool {
		polls.Add(1)
		return false
	}, func() string {
		describeCalls.Add(1)
		return fmt.Sprintf("state after %d polls", polls.Load())
	})

	if !rec.failed {
		t.Fatal("EventuallyEveryFunc did not call Fatalf on deadline")
	}
	if rec.helperCalls == 0 {
		t.Error("EventuallyEveryFunc did not call t.Helper()")
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("EventuallyEveryFunc gave up after %s, before the %s timeout", elapsed, timeout)
	}
	if got := describeCalls.Load(); got != 1 {
		t.Errorf("describe called %d times, want exactly once (it may do real work: render a frame, dump a queue)", got)
	}
	finalPolls := polls.Load()
	if finalPolls < 2 {
		t.Fatalf("cond polled only %d times in %s at a 5ms interval; the wait did not sample", finalPolls, timeout)
	}
	if strings.Contains(rec.fatal, "state after 0 polls") {
		t.Errorf("failure message describes t=0 — the description was captured eagerly:\n%s", rec.fatal)
	}
	for _, want := range []string{
		fmt.Sprintf("state after %d polls", finalPolls), // the deadline-time state
		timeout.String(), // how long it waited
	} {
		if !strings.Contains(rec.fatal, want) {
			t.Errorf("Fatalf message %q does not contain %q", rec.fatal, want)
		}
	}
}
