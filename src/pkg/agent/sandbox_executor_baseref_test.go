package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

// Regression coverage for kubestellar/hive#4928 on the sandbox path: the
// executor branched from, and opened its PR against, a hardcoded "main"
// regardless of what the target repository's default branch actually is. `git
// clone` already records the remote's HEAD, so the right answer was on disk the
// whole time.

// baseRefRunner is a git stub that reports a configurable default branch for
// refs/remotes/origin/HEAD and records the ref the executor fetched.
type baseRefRunner struct {
	mu sync.Mutex

	symbolicRef    string // what `git symbolic-ref --short refs/remotes/origin/HEAD` prints
	symbolicRefErr error  // when set, the lookup fails instead

	fetchedRef      string
	symbolicRefRuns int
	revParses       int
}

func (r *baseRefRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name != "git" {
		return nil, errors.New("unexpected command")
	}
	effective := stripGitConfigArgs(args)
	joined := strings.Join(effective, " ")
	switch {
	case len(effective) >= 4 && effective[0] == "clone":
		workspace := effective[len(effective)-1]
		return nil, os.MkdirAll(filepath.Join(workspace, ".git"), 0o770)
	case joined == "symbolic-ref --short refs/remotes/origin/HEAD":
		r.mu.Lock()
		r.symbolicRefRuns++
		r.mu.Unlock()
		if r.symbolicRefErr != nil {
			return nil, r.symbolicRefErr
		}
		return []byte(r.symbolicRef + "\n"), nil
	case strings.HasPrefix(joined, "fetch origin "):
		r.mu.Lock()
		r.fetchedRef = strings.TrimPrefix(joined, "fetch origin ")
		r.mu.Unlock()
		return []byte("ok\n"), nil
	case joined == "rev-parse HEAD":
		// First answer is the base the workspace was branched from; later ones
		// are the agent's commit, so commitsSince sees real work to push.
		r.mu.Lock()
		r.revParses++
		n := r.revParses
		r.mu.Unlock()
		if n > 1 {
			return []byte("headsha\n"), nil
		}
		return []byte("basesha\n"), nil
	case joined == "rev-list --count basesha..HEAD":
		return []byte("1\n"), nil
	case joined == "rev-parse --verify basesha":
		return []byte("basesha\n"), nil
	case joined == "diff --name-only basesha...HEAD":
		return []byte("file.txt\n"), nil
	case joined == "diff --no-ext-diff basesha...HEAD":
		return []byte("diff --git a/file.txt b/file.txt\n"), nil
	case strings.Contains(joined, "push ") && strings.Contains(joined, " origin HEAD:refs/heads/"):
		return []byte("pushed\n"), nil
	default:
		return []byte(""), nil
	}
}

// baseRefPRClient records the base the executor asked the hive to open against.
type baseRefPRClient struct{ base string }

func (p *baseRefPRClient) CreatePR(_ context.Context, _, _, base, _, _ string) (ghpkg.CreatePRResult, error) {
	p.base = base
	return ghpkg.CreatePRResult{Number: 1, URL: "https://example.test/pr/1"}, nil
}

func runSandboxForBaseRef(t *testing.T, runner *baseRefRunner, specBase string) *baseRefPRClient {
	t.Helper()
	pr := &baseRefPRClient{}
	exec := &SandboxExecutor{
		Runner:      runner,
		Launcher:    sandboxFakeLauncher{},
		PRClient:    pr,
		PushEnabled: true,
		Minter:      sandboxFakeMinter{},
	}
	if _, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "scanner", AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
		Org: "kubestellar", Repo: "hive", BaseRef: specBase,
		WorkspaceDir: t.TempDir(), Image: "agent-image",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return pr
}

func TestSandboxExecutorBranchesFromRepoDefaultBranch(t *testing.T) {
	runner := &baseRefRunner{symbolicRef: "origin/testing"}
	pr := runSandboxForBaseRef(t, runner, "")

	if runner.fetchedRef != "testing" {
		t.Fatalf("fetched %q, want testing — the clone's own default branch", runner.fetchedRef)
	}
	if pr.base != "testing" {
		t.Fatalf("PR opened against %q, want testing", pr.base)
	}
}

func TestSandboxExecutorFallsBackWhenDefaultBranchUnreadable(t *testing.T) {
	runner := &baseRefRunner{symbolicRefErr: errors.New("no origin/HEAD")}
	pr := runSandboxForBaseRef(t, runner, "")

	if runner.fetchedRef != "main" {
		t.Fatalf("fetched %q, want the main fallback", runner.fetchedRef)
	}
	if pr.base != "main" {
		t.Fatalf("PR opened against %q, want the main fallback", pr.base)
	}
}

func TestSandboxExecutorHonorsPinnedBaseRef(t *testing.T) {
	runner := &baseRefRunner{symbolicRef: "origin/testing"}
	pr := runSandboxForBaseRef(t, runner, "release-1.2")

	if runner.fetchedRef != "release-1.2" {
		t.Fatalf("fetched %q, want the pinned release-1.2", runner.fetchedRef)
	}
	if pr.base != "release-1.2" {
		t.Fatalf("PR opened against %q, want release-1.2", pr.base)
	}
	if runner.symbolicRefRuns != 0 {
		t.Fatalf("pinned base still ran %d default-branch lookups, want 0", runner.symbolicRefRuns)
	}
}
