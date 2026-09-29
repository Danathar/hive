//go:build windows

package dashboard

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

var errSpekHubFenceBusy = errors.New("Spektacular worktree is owned by another process")

type spekHubFenceContextKey struct{}

// Refuse execution where inheritable worktree fencing is not implemented.
func acquireSpekHubFence(string) (*os.File, error) {
	return nil, errors.New("Spektacular hub execution requires Unix worktree fencing")
}
func spekHubInheritFence(*exec.Cmd, context.Context) {}
