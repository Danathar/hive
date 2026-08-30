package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var (
	outreachClaimEvidenceHeading = regexp.MustCompile(`(?im)^#{1,6}[ \t]+claim evidence[ \t]*\r?$`)
	outreachRegulatoryTerm       = regexp.MustCompile(`(?i)\b(?:hipaa|gdpr|fedramp|fips(?:[- ]?\d+)?|soc[- ]?2|pci(?:[- ]?dss)?)\b`)
)

// validateOutreachPRRequest is the mechanical backstop for the outreach
// policy's public-claims boundary. Prompt instructions remain responsible for
// verifying capabilities against primary project evidence; this choke point
// makes that verification visible to reviewers and prevents the highest-risk
// regulatory language from becoming public in an agent-authored PR at all.
//
// A non-empty reason is a permanent policy rejection. An error is a transient
// forge failure and should use the request watcher's normal bounded retry path.
func (c *Client) validateOutreachPRRequest(ctx context.Context, req PRRequest) (reason string, err error) {
	if !strings.EqualFold(strings.TrimSpace(req.Agent), "outreach") {
		return "", nil
	}
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	if !outreachClaimEvidenceHeading.MatchString(req.Body) {
		return "outreach PR body must include a Claim evidence section mapping public capability and roadmap claims to primary project sources", nil
	}
	if term := outreachRegulatoryTerm.FindString(req.Title + "\n" + req.Body); term != "" {
		return fmt.Sprintf("outreach PR metadata contains regulatory term %q; regulatory and compliance language must be handled by a human", term), nil
	}

	owner, repo := c.requestRepo(req.Repo)
	base := strings.TrimSpace(req.Base)
	if base == "" {
		base, err = c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return "", fmt.Errorf("resolving default branch for outreach claim validation: %w", err)
		}
	}

	comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, strings.TrimSpace(req.Head), nil)
	if err != nil {
		return "", fmt.Errorf("comparing outreach branch %s...%s for claim validation: %w", base, req.Head, err)
	}
	if comparison == nil {
		return "", fmt.Errorf("comparing outreach branch %s...%s for claim validation: GitHub returned an empty comparison", base, req.Head)
	}
	// GitHub's compare response exposes at most 300 changed files. Refuse a
	// boundary-sized response rather than silently approving uninspected files;
	// outreach changes should be small enough for a human-readable review.
	if len(comparison.Files) >= 300 {
		return "outreach diff is too large to validate completely; split it into reviewable PRs", nil
	}

	for _, file := range comparison.Files {
		if file == nil || file.GetAdditions() == 0 {
			continue
		}
		patch := file.GetPatch()
		if patch == "" {
			return fmt.Sprintf("cannot inspect added text in %s; open this change manually for human review", file.GetFilename()), nil
		}
		for _, line := range strings.Split(patch, "\n") {
			if !strings.HasPrefix(line, "+") {
				continue
			}
			if term := outreachRegulatoryTerm.FindString(line[1:]); term != "" {
				return fmt.Sprintf("outreach content adds regulatory term %q in %s; regulatory and compliance language must be handled by a human", term, file.GetFilename()), nil
			}
		}
	}
	return "", nil
}

func (c *Client) requestRepo(repo string) (string, string) {
	owner := c.org
	if parts := strings.SplitN(strings.TrimSpace(repo), "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return owner, strings.TrimSpace(repo)
}
