package github

import (
	"context"
	"log/slog"
	"strings"
)

// FallbackDefaultBranch is what DefaultBranch reports when the repository's real
// default branch cannot be resolved (no client, no credentials, or an API error).
//
// It is a LAST resort, not a starting assumption. Hive used to hardcode it as
// THE base for every PR it opened, which silently mis-targeted every repository
// whose default branch is not "main": the PR's diff then carries the entire
// divergence between the two branches, and merging it lands the change on the
// wrong branch (kubestellar/hive#4928).
const FallbackDefaultBranch = "main"

// DefaultBranch resolves the default branch of owner/repo from the repository's
// own metadata (the `default_branch` REST field — the API behind
// `gh repo view --json defaultBranchRef`), which the App's metadata:read
// permission already covers.
//
// The result is cached for the life of the process: a repo's default branch
// changes about as often as the repo is renamed, so caching keeps this off the
// hot path of every PR open. Only successful lookups are cached — a transient
// API error must not pin the repo to the fallback until the next restart.
//
// It never fails: an unresolvable repo falls back to FallbackDefaultBranch and
// logs, because a base that is probably right beats refusing to open the PR at
// all. Callers that need to distinguish "resolved" from "guessed" should call
// GetRepo directly.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) string {
	if c == nil || c.client == nil {
		return FallbackDefaultBranch
	}
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return FallbackDefaultBranch
	}
	key := owner + "/" + repo
	if branch, ok := c.cachedDefaultBranch(key); ok {
		return branch
	}

	r, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("DefaultBranch: lookup failed, falling back",
				slog.String("repo", key),
				slog.String("fallback", FallbackDefaultBranch),
				slog.String("error", err.Error()))
		}
		return FallbackDefaultBranch
	}
	branch := strings.TrimSpace(r.GetDefaultBranch())
	if branch == "" {
		// A repo with no default_branch is not a thing GitHub returns for a
		// live repo, but an empty string here would cache a base of "" and
		// make every later PR open fail — treat it as unresolved.
		return FallbackDefaultBranch
	}
	c.storeDefaultBranch(key, branch)
	return branch
}

func (c *Client) cachedDefaultBranch(key string) (string, bool) {
	c.defaultBranchMu.RLock()
	defer c.defaultBranchMu.RUnlock()
	branch, ok := c.defaultBranches[key]
	return branch, ok
}

func (c *Client) storeDefaultBranch(key, branch string) {
	c.defaultBranchMu.Lock()
	defer c.defaultBranchMu.Unlock()
	if c.defaultBranches == nil {
		c.defaultBranches = map[string]string{}
	}
	c.defaultBranches[key] = branch
}
