// Package forge provides a source-forge abstraction so Hive is not hardcoded
// to GitHub. It defines a small, forge-neutral Forge interface and neutral data
// types (Repo, Issue, ChangeRequest), plus concrete adapters for GitHub (a thin
// wrapper over pkg/github) and GitLab (a stdlib net/http client for the REST v4
// API).
//
// This is a FIRST SLICE: the read path (enumerate open issues / merge requests,
// get a repo) is implemented and tested for both forges. The write path
// (commenting, labelling, merging) is intentionally left as clearly-marked
// TODOs on the interface rather than half-implemented — see the "Write path"
// section of the Forge interface. Callers that need writes today should continue
// to use *github.Client directly until those methods are filled in.
package forge

import (
	"context"
	"fmt"
	"time"
)

// Kind identifies a supported forge implementation.
type Kind string

const (
	// KindGitHub selects the GitHub adapter (wraps pkg/github).
	KindGitHub Kind = "github"
	// KindGitLab selects the GitLab adapter (REST API v4 over net/http).
	KindGitLab Kind = "gitlab"
)

// Repo is a forge-neutral description of a repository / project.
type Repo struct {
	// FullName is the human "owner/name" (GitHub) or "namespace/path" (GitLab)
	// slug that identifies the repo within its forge.
	FullName string `json:"full_name"`
	// Owner is the org/namespace portion of FullName.
	Owner string `json:"owner"`
	// Name is the repository/project name portion of FullName.
	Name string `json:"name"`
	// URL is the canonical web URL of the repo.
	URL string `json:"url"`
	// DefaultBranch is the repo's default branch (may be empty if unknown).
	DefaultBranch string `json:"default_branch,omitempty"`
	// Description is the repo's short description (may be empty).
	Description string `json:"description,omitempty"`
}

// Issue is a forge-neutral issue. It maps GitHub issues and GitLab issues onto
// one shape. Fields absent on a given forge are left at their zero value.
type Issue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Assignees []string  `json:"assignees"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
}

// ChangeRequest is a forge-neutral pull request (GitHub) / merge request
// (GitLab). The name is deliberately forge-neutral because "pull request" is a
// GitHub term.
type ChangeRequest struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Draft     bool      `json:"draft"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
	// SourceBranch / TargetBranch describe the change's direction. GitHub calls
	// these head/base; GitLab calls them source/target. Empty when unknown.
	SourceBranch string `json:"source_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	// HeadSHA is the tip commit of the change (GitHub head SHA / GitLab sha).
	HeadSHA string `json:"head_sha,omitempty"`
}

// Forge is the forge-neutral operation set Hive depends on. It is intentionally
// minimal for this first slice.
//
// Read path — implemented by both the GitHub and GitLab adapters:
//   - Kind
//   - GetRepo
//   - ListOpenIssues
//   - ListOpenChangeRequests
//
// Write path — NOT part of this interface yet. Adding write operations
// (comment, add/remove label, merge, hold/unhold) is deliberately deferred so
// this slice compiles and is fully tested rather than broad-but-broken. When
// added they should be forge-neutral, e.g.:
//
//	// TODO(forge write path): AddComment(ctx, repo string, number int, body string) error
//	// TODO(forge write path): AddLabels(ctx, repo string, number int, labels []string) error
//	// TODO(forge write path): RemoveLabel(ctx, repo string, number int, label string) error
//	// TODO(forge write path): Merge(ctx, repo string, number int, opts MergeOptions) error
//
// GitHub already has concrete implementations of all of the above on
// *github.Client; the write-path work is to lift those onto this interface and
// implement the GitLab equivalents against the REST v4 API.
type Forge interface {
	// Kind reports which forge implementation this is.
	Kind() Kind

	// GetRepo fetches a single repo by its forge slug ("owner/name" for GitHub,
	// "namespace/path" for GitLab).
	GetRepo(ctx context.Context, repo string) (*Repo, error)

	// ListOpenIssues returns the open issues for a repo. On GitHub this excludes
	// pull requests (which the REST API models as issues); on GitLab issues and
	// merge requests are already separate resources.
	ListOpenIssues(ctx context.Context, repo string) ([]Issue, error)

	// ListOpenChangeRequests returns the open pull requests (GitHub) / merge
	// requests (GitLab) for a repo.
	ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error)
}

// Options configures a forge at construction time. All fields are optional; a
// zero Options yields sensible defaults (public forge endpoints).
type Options struct {
	// BaseURL overrides the forge API base URL. For GitHub this is the REST API
	// URL (e.g. GHE "https://github.example.com/api/v3"); empty uses the public
	// default. For GitLab this is the instance root (e.g.
	// "https://gitlab.example.com"); empty uses "https://gitlab.com". The GitLab
	// adapter appends the "/api/v4" path itself.
	BaseURL string
	// Org is the default owner/namespace used when a repo slug has no "/" prefix.
	Org string
}

// NewForge constructs a Forge of the given kind.
//
// token authenticates against the forge. It must be supplied by the caller
// (typically from an environment variable such as GITHUB_TOKEN / GITLAB_TOKEN);
// this package never reads or defaults secrets itself.
//
// For KindGitHub this builds a thin adapter over pkg/github.NewClient. For
// KindGitLab this builds the stdlib REST v4 adapter.
func NewForge(kind Kind, token string, opts Options) (Forge, error) {
	switch kind {
	case KindGitHub, Kind(""):
		// Empty kind defaults to GitHub so existing GitHub-only configs keep
		// working without naming a forge.
		return newGitHubForge(token, opts), nil
	case KindGitLab:
		return newGitLabForge(token, opts)
	default:
		return nil, fmt.Errorf("unknown forge kind %q (want %q or %q)", kind, KindGitHub, KindGitLab)
	}
}

// splitRepo splits an "owner/name" slug into its parts, falling back to
// defaultOwner when no "/" is present. It is shared by the adapters.
func splitRepo(repo, defaultOwner string) (owner, name string) {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:]
		}
	}
	return defaultOwner, repo
}
