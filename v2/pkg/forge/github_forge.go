package forge

import (
	"context"
	"fmt"
	"log/slog"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/github"
)

// githubReader is the subset of *github.Client the GitHub adapter needs for the
// read path. Depending on an interface (rather than the concrete client) lets
// tests inject a stub without real network calls, and keeps this adapter a thin
// delegator over the existing client — no logic is duplicated here.
type githubReader interface {
	GetRepo(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error)
	EnumerateActionable(ctx context.Context) (*github.ActionableResult, error)
}

// gitHubForge adapts the existing pkg/github client to the Forge interface.
//
// It is deliberately thin: read operations delegate to the underlying client
// and translate its already-neutralized types (github.Issue/PullRequest) into
// the forge-neutral types. The write path is not implemented here — callers that
// need writes continue to use *github.Client directly until the Forge write
// path (see TODOs on the Forge interface) lands.
type gitHubForge struct {
	client githubReader
	org    string
}

// newGitHubForge builds a GitHub adapter backed by a real pkg/github client.
func newGitHubForge(token string, opts Options) *gitHubForge {
	// A single-repo enumeration is not what EnumerateActionable does, so the
	// underlying client is constructed with no fixed repo list; per-repo reads
	// go through the go-github client via GetRepo. The org is used to resolve
	// bare repo names.
	client := github.NewClient(token, opts.Org, nil, slog.Default(), opts.BaseURL)
	return &gitHubForge{client: client, org: opts.Org}
}

// newGitHubForgeWithReader is a test seam: it builds the adapter over an
// arbitrary githubReader (e.g. a stub), bypassing real client construction.
func newGitHubForgeWithReader(r githubReader, org string) *gitHubForge {
	return &gitHubForge{client: r, org: org}
}

func (f *gitHubForge) Kind() Kind { return KindGitHub }

func (f *gitHubForge) GetRepo(ctx context.Context, repo string) (*Repo, error) {
	owner, name := splitRepo(repo, f.org)
	r, _, err := f.client.GetRepo(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("github: get repo %s/%s: %w", owner, name, err)
	}
	if r == nil {
		return nil, fmt.Errorf("github: get repo %s/%s: empty response", owner, name)
	}
	return &Repo{
		FullName:      r.GetFullName(),
		Owner:         owner,
		Name:          name,
		URL:           r.GetHTMLURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Description:   r.GetDescription(),
	}, nil
}

// ListOpenIssues delegates to EnumerateActionable and filters to the requested
// repo, reusing the existing client's issue-neutralization (author, labels,
// assignees, PR exclusion) rather than re-implementing it here.
func (f *gitHubForge) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	res, err := f.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: list issues for %s: %w", repo, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]Issue, 0, len(res.Issues.Items))
	for _, it := range res.Issues.Items {
		if repo != "" && it.Repo != repo {
			continue
		}
		out = append(out, Issue{
			Repo:      it.Repo,
			Number:    it.Number,
			Title:     it.Title,
			Author:    it.Author,
			Labels:    it.Labels,
			Assignees: it.Assignees,
			State:     "open",
			CreatedAt: it.CreatedAt,
			URL:       it.URL,
		})
	}
	return out, nil
}

// ListOpenChangeRequests delegates to EnumerateActionable and filters to the
// requested repo.
func (f *gitHubForge) ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error) {
	res, err := f.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: list change requests for %s: %w", repo, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]ChangeRequest, 0, len(res.PRs.Items))
	for _, pr := range res.PRs.Items {
		if repo != "" && pr.Repo != repo {
			continue
		}
		out = append(out, ChangeRequest{
			Repo:      pr.Repo,
			Number:    pr.Number,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Draft:     pr.Draft,
			State:     "open",
			CreatedAt: pr.CreatedAt,
			URL:       pr.URL,
			HeadSHA:   pr.HeadSHA,
		})
	}
	return out, nil
}

// Compile-time assertion that the adapter satisfies the interface.
var _ Forge = (*gitHubForge)(nil)
