// Package testutil holds helpers shared by the unit tests under pkg/ and cmd/.
// It is test-support code only: nothing under internal/testutil is imported by
// a production package.
package testutil

import (
	"fmt"
	"testing"
	"time"
)

// pollInterval is how often Eventually re-evaluates its condition. Short
// enough that a condition which becomes true quickly is observed within a few
// milliseconds; long enough not to burn a core while waiting.
const pollInterval = 10 * time.Millisecond

// Eventually polls cond until it returns true or timeout elapses. On the
// deadline it fails the test via t.Fatalf with msg (a printf format) and args.
//
// Use Eventually instead of a fixed time.Sleep whenever a test is waiting for
// something OBSERVABLE to happen — a goroutine to bump a counter, a file to
// appear, a connection to register — and then asserts on it. A fixed sleep is
// a timing margin: it always waits the full duration on a fast machine and is
// still too short on a loaded CI runner (that is the flake). Eventually waits
// exactly as long as needed, up to the timeout, which can therefore be
// generous without costing wall clock on the happy path.
//
// Eventually is NOT the right tool when:
//
//   - the test proves that something does NOT happen ("no second reload after
//     the debounce window"). A negative wait must span the whole window; keep
//     the sleep and say so in a comment.
//   - the code under test sleeps or ticks on its own clock and the test cares
//     about event ORDER, not wall time. Wrap the body in testing/synctest.Test
//     (Go 1.25): inside the bubble time.Sleep and timers are virtual,
//     synctest.Wait blocks until every goroutine in the bubble is durably
//     blocked, and the whole thing completes in microseconds with no polling.
//     synctest needs the code under test to be bubble-friendly (no real I/O or
//     exec on goroutines started inside the bubble, no comparisons against a
//     wall-clock captured outside it), so it is a refactor rather than a
//     drop-in; reach for it when a test's only dependency on time is the
//     code's own timers.
//   - a goroutine you started signals completion. Close a channel or use a
//     sync.WaitGroup; polling for a signal you can receive directly is
//     strictly worse.
func Eventually(t testing.TB, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	EventuallyEvery(t, timeout, pollInterval, cond, msg, args...)
}

// EventuallyEvery is Eventually with a caller-chosen sampling interval.
//
// The default 10ms suits a condition that becomes true after real work — a
// server responding, a file appearing. It is far too coarse for an in-process
// harness whose state changes in microseconds: sampling ten times slower than
// the thing you are watching turns a fast suite into a slow one, and a suite
// that takes minutes stops being run under -count=N, which is where genuine
// flakes surface.
//
// Prefer plain Eventually. Reach for this only when you have measured that
// the default interval dominates the wait, and say so at the call site.
func EventuallyEvery(t testing.TB, timeout, interval time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (condition not met within %s)", fmt.Sprintf(msg, args...), timeout)
			return
		}
		time.Sleep(interval)
	}
}

// EventuallyValue polls get until it reports ok and returns the value it
// produced, failing the test via t.Fatalf on timeout exactly like Eventually.
// Use it when the test needs the observed value for a subsequent assertion
// (a snapshot pulled out from under a mutex, the entry that finally appeared)
// rather than just the fact that it appeared.
func EventuallyValue[T any](t testing.TB, timeout time.Duration, get func() (T, bool), msg string, args ...any) T {
	t.Helper()
	var got T
	Eventually(t, timeout, func() bool {
		v, ok := get()
		if ok {
			got = v
		}
		return ok
	}, msg, args...)
	return got
}

// EventuallyEveryFunc is EventuallyEvery with the failure message produced
// LAZILY, at the deadline, instead of being formatted by the caller up front.
//
// It exists because a diagnostic captured eagerly is a diagnostic about the
// wrong moment. Passing a rendered snapshot of the system into EventuallyEvery
// as a printf argument:
//
//	EventuallyEvery(t, d, i, cond, "timed out\n%s", h.render())
//
// looks deferred — the formatting genuinely is — but h.render() is evaluated
// BEFORE the wait starts. When the condition then never holds, the failure
// prints the state the system was in at t=0 rather than at the timeout. For a
// wait that hangs, that is the difference between seeing why it hung and
// seeing a snapshot that has long since been superseded. This bit a TUI
// acceptance harness where every hang reported the startup frame.
//
// describe is called at most once, only on the failure path, so it is free to
// do real work: render a frame, dump a queue, walk recorded traffic.
func EventuallyEveryFunc(t testing.TB, timeout, interval time.Duration, cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (condition not met within %s)", describe(), timeout)
			return
		}
		time.Sleep(interval)
	}
}
