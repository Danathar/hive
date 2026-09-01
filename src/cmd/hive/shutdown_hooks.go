package main

import "sync"

// shutdownHooks is the ordered set of functions the signal handler runs before
// the root context is canceled — while every WebSocket, tmux server and PVC
// mount is still live.
//
// WHY A SLICE (kubestellar/hive#5390). This began as a single
// atomic.Pointer[func()]. One pointer means one hook, and registration is
// therefore DESTRUCTIVE: whoever calls Store second silently discards whatever
// was there first. Nothing errors, no test fails, a shutdown side effect just
// stops happening. #4296's kick-log archive already held the slot, so adding
// the contributor-socket drain to it would have quietly deleted the archive on
// every pod roll — invisible, and only discovered when someone went looking for
// scrollback that was no longer being written. Appending cannot do that.
//
// Hooks run SERIALLY in registration order, on the signal goroutine, and each
// is responsible for bounding its own work: this type imposes no timeout,
// because a shared budget here would silently truncate a later hook when an
// earlier one ran long. The whole sequence is racing
// terminationGracePeriodSeconds (30s), so a hook that can block against a
// remote peer must carry its own deadline — see wsDrainBudget.
//
// A panic in one hook must not cost the others theirs: the sequence is
// best-effort cleanup on a process that is exiting regardless, so a hook that
// blows up is contained and the rest still run.
type shutdownHooks struct {
	mu    sync.Mutex
	hooks []namedShutdownHook
}

type namedShutdownHook struct {
	name string
	fn   func()
}

// add registers a hook to run at shutdown. The name is used only for panic
// reporting. A nil fn is ignored so a caller need not guard an optional
// subsystem at the registration site.
func (s *shutdownHooks) add(name string, fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, namedShutdownHook{name: name, fn: fn})
}

// addUrgent registers a hook to run BEFORE every hook registered so far.
//
// Registration order in main() is dictated by construction order — a hook
// cannot be registered before the subsystem it touches exists — and that has
// nothing to do with what should run first at shutdown. The contributor drain
// is constructed late (the dashboard server comes ~350 lines after the agent
// manager) but is the time-critical hook: it puts a Close frame on the wire
// that a relay is waiting on, while the kick-log archive it would otherwise
// queue behind does PVC I/O on NFS with nobody waiting. This lets a late
// registration still take the front of the queue.
func (s *shutdownHooks) addUrgent(name string, fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append([]namedShutdownHook{{name: name, fn: fn}}, s.hooks...)
}

// run executes every registered hook in registration order. It is safe to call
// on a zero value and with no hooks registered.
func (s *shutdownHooks) run() {
	s.mu.Lock()
	hooks := make([]namedShutdownHook, len(s.hooks))
	copy(hooks, s.hooks)
	s.mu.Unlock()

	for _, h := range hooks {
		runShutdownHook(h)
	}
}

// runShutdownHook isolates one hook's panic so a later hook still runs.
func runShutdownHook(h namedShutdownHook) {
	defer func() {
		_ = recover()
	}()
	h.fn()
}

// len reports how many hooks are registered. Test-facing.
func (s *shutdownHooks) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hooks)
}
