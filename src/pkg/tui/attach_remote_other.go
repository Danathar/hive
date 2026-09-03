//go:build !unix

package tui

import "os"

// stdinPollingSupported: no poll(2) here, so Run routes stdin to the blocking
// pump and never waits on it.
const stdinPollingSupported = false

// pumpFile is unreachable when stdinPollingSupported is false; it exists so
// attach_remote.go compiles identically on every platform.
func pumpFile(_ <-chan struct{}, _ *os.File, _ func([]byte) error) {}

// watchResize is a no-op where SIGWINCH does not exist. The remote session
// keeps the size it was started with; ttyd's protocol has no way to learn of
// a resize the platform cannot report.
func watchResize(done <-chan struct{}, _ *os.File, _ func(cols, rows int) error) {
	<-done
}
