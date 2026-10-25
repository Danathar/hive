package github

// Park re-check: an issue parked for a person (stage/045-needs-decision) is
// parked on a premise — "#722 is in progress, agents leave it alone", "the
// precedent has not been set", "whether to adopt ruff and type checking".
// The park text is written once, usually by a contributor relay under its
// own account, and nothing re-read it when the work it named moved on. A
// maintainer then decides from a premise that no longer holds, and agents
// stay off work that is unblocked (projectbluefin/server#385,
// projectbluefin/utah#13, projectbluefin/website#926).
//
// RecheckParkedIssues re-reads each open parked issue's timeline. A change
// counts when, after the park began,
//
//   - a pull request or issue the park text names merged or closed, or
//   - a pull request that refers to the issue ("Refs #N", a closing keyword,
//     "part of", …) merged.
//
// A PR that only lists the issue in passing is not a change: a person's
// intake table once linked ~65 issues (see hard_suppress_reconcile.go).
//
// When something changed, the hive posts one note under its own marker,
// listing what changed, and edits it in place as more lands. It never
// un-parks: leaving 045 stays a maintainer's reply (labels-and-control-
// signals.md), and a park can rest on a maintainer's explicit request that
// no merged PR answers (website#926 cited one).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// parkRecheckMarker opens the hive's one re-check note per parked issue.
const parkRecheckMarker = "<!-- hive:park-recheck -->"

// parkRelabelGap bridges a park label swapped for another park label (the
// needs-decision → stage/085 → stage/045 migrations removed one and added the
// next a second apart): a gap this short does not start a new park.
const parkRelabelGap = 15 * time.Minute

// parkTextWindow is how long before the park label the park text may have
// been posted: the relay posts its comment, then the hive applies the label.
const parkTextWindow = 15 * time.Minute

// parkRecheckMaxNamedLookups bounds the per-issue reads for items the park
// text names that do not cross-reference the issue.
const parkRecheckMaxNamedLookups = 10

// ParkRecheckConfig scopes one pass.
type ParkRecheckConfig struct {
	Repos []string
}

// ParkRecheckResult reports one pass.
type ParkRecheckResult struct {
	Posted  int      `json:"posted"`
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	Items   []string `json:"items,omitempty"`
}

// parkRef is one item a park depends on, as the timeline reports it now.
type parkRef struct {
	repo     string // owner/repo
	number   int
	isPR     bool
	state    string
	mergedAt time.Time
	closedAt time.Time
	body     string
}

// parkChange is one thing that moved after the park began.
type parkChange struct {
	ref   string // "#409", or "owner/repo#N" outside the issue's repo
	verb  string // "merged", "was closed without merging", "was closed"
	at    time.Time
	named bool // the park text names it; else it refers to the issue
}

// RecheckParkedIssues posts or refreshes the re-check note on every open
// issue parked at the configured needs-decision label whose park premise
// moved. See the file comment.
func (c *Client) RecheckParkedIssues(ctx context.Context, cfg ParkRecheckConfig) (*ParkRecheckResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	result := &ParkRecheckResult{}
	label := c.needsDecisionLabel()
	var firstErr error
	for _, repoRef := range cfg.Repos {
		owner, repo := c.splitRepo(repoRef)
		if owner == "" || repo == "" {
			continue
		}
		issues, err := c.listOpenIssuesWithLabel(ctx, owner, repo, label)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			if err := c.recheckParkedIssue(ctx, owner, repo, issue, label, result); err != nil {
				if c.logger != nil {
					c.logger.Warn("park re-check skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
			}
		}
	}
	return result, firstErr
}

func (c *Client) recheckParkedIssue(ctx context.Context, owner, repo string, issue *gh.Issue, label string, result *ParkRecheckResult) error {
	number := issue.GetNumber()
	events, err := c.listIssueTimelineAll(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	start := parkStartFrom(events, c.isDecisionParkLabel)
	if start.IsZero() {
		result.Skipped++
		return nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	refs := parkRefsFromTimeline(events)
	named := parkNamedRefs(parkText(issue, comments, start), owner+"/"+repo, number)
	lookups := 0
	for _, key := range named {
		if _, ok := refs[key]; ok || lookups >= parkRecheckMaxNamedLookups {
			continue
		}
		lookups++
		if ref, ok := c.readParkRef(ctx, key); ok {
			refs[key] = ref
		}
	}
	changes := parkChanges(refs, named, owner+"/"+repo, number, start)
	if len(changes) == 0 {
		result.Skipped++
		return nil
	}
	note := parkRecheckNote(label, start, changes)
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) || !strings.Contains(cm.GetBody(), parkRecheckMarker) {
			continue
		}
		if cm.GetBody() == note {
			result.Skipped++
			return nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(note)}); err != nil {
			return fmt.Errorf("editing park re-check note on %s/%s#%d: %w", owner, repo, number, err)
		}
		result.Updated++
		result.Items = append(result.Items, fmt.Sprintf("%s/%s#%d", owner, repo, number))
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(note)}); err != nil {
		return fmt.Errorf("posting park re-check note on %s/%s#%d: %w", owner, repo, number, err)
	}
	result.Posted++
	result.Items = append(result.Items, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	if c.logger != nil {
		c.logger.Info("PARK-RECHECK", "repo", repo, "issue", number, "parked_since", start.UTC().Format(time.RFC3339), "changes", len(changes))
	}
	return nil
}

// isDecisionParkLabel reports whether label parks an issue for a person's
// decision. `blocked` parks on an outside blocker and is reconciled on its
// own (ReconcileBlockedIssues), so it does not start a decision park.
func (c *Client) isDecisionParkLabel(label string) bool {
	return !strings.EqualFold(strings.TrimSpace(label), issueBlockedLabel) && c.isParkingLabel(label)
}

func (c *Client) listIssueTimelineAll(ctx context.Context, owner, repo string, number int) ([]*gh.Timeline, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []*gh.Timeline
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing timeline for %s/%s#%d: %w", owner, repo, number, err)
		}
		out = append(out, events...)
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// readParkRef reads one named item's current state; ok is false when it
// cannot be read (a typo, a deleted item, no access).
func (c *Client) readParkRef(ctx context.Context, key string) (parkRef, bool) {
	repoFull, n, ok := splitParkRefKey(key)
	if !ok {
		return parkRef{}, false
	}
	owner, repo := c.splitRepo(repoFull)
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, n)
	if err != nil || issue == nil {
		return parkRef{}, false
	}
	ref := parkRef{repo: repoFull, number: n, isPR: issue.IsPullRequest(), state: issue.GetState(), closedAt: issue.GetClosedAt().Time, body: issue.GetBody()}
	if ref.isPR {
		ref.mergedAt = issue.GetPullRequestLinks().GetMergedAt().Time
	}
	return ref, true
}

// parkStartFrom returns when the issue's current park began: the first park
// label of the run that is still on the issue, bridging label swaps shorter
// than parkRelabelGap. Zero when the timeline shows no park in force.
func parkStartFrom(events []*gh.Timeline, isPark func(string) bool) time.Time {
	present := map[string]bool{}
	var start, emptySince time.Time
	for _, ev := range events {
		if ev == nil || ev.Label == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ev.GetLabel().GetName()))
		if name == "" || !isPark(name) {
			continue
		}
		at := ev.GetCreatedAt().Time
		switch strings.ToLower(ev.GetEvent()) {
		case "labeled":
			if len(present) == 0 && (start.IsZero() || emptySince.IsZero() || at.Sub(emptySince) > parkRelabelGap) {
				start = at
			}
			present[name] = true
		case "unlabeled":
			delete(present, name)
			if len(present) == 0 {
				emptySince = at
			}
		}
	}
	if len(present) == 0 {
		return time.Time{}
	}
	return start
}

// parkText is the text the park rests on: the issue body when the issue was
// filed parked, and every comment posted in the window before the park
// label. The hive's own notices are not park text.
func parkText(issue *gh.Issue, comments []*gh.IssueComment, start time.Time) string {
	inWindow := func(at time.Time) bool {
		return !at.IsZero() && !at.After(start) && start.Sub(at) <= parkTextWindow
	}
	var parts []string
	if inWindow(issue.GetCreatedAt().Time) {
		parts = append(parts, issue.GetBody())
	}
	for _, cm := range comments {
		if cm == nil || !inWindow(cm.GetCreatedAt().Time) {
			continue
		}
		body := cm.GetBody()
		if strings.Contains(body, unparkNoticeMarker) || strings.Contains(body, parkRecheckMarker) {
			continue
		}
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n")
}

// parkRefRE matches "#N" and "owner/repo#N" references.
var parkRefRE = regexp.MustCompile(`(?:^|[^\w/&#])(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#(\d+)\b`)

// parkNamedRefs returns the "owner/repo#N" keys the park text names, in
// order of first mention, without the issue itself.
func parkNamedRefs(text, issueRepo string, number int) []string {
	seen := map[string]bool{parkRefKey(issueRepo, number): true}
	var out []string
	for _, m := range parkRefRE.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			continue
		}
		repo := issueRepo
		if m[1] != "" {
			repo = m[1]
		}
		key := parkRefKey(repo, n)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func parkRefKey(repo string, n int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(n)
}

func splitParkRefKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return key[:i], n, true
}

// parkRefsFromTimeline indexes the items that cross-reference the issue by
// key, with their current state as the timeline reports it.
func parkRefsFromTimeline(events []*gh.Timeline) map[string]parkRef {
	out := map[string]parkRef{}
	for _, ev := range events {
		if ev == nil || !strings.EqualFold(ev.GetEvent(), "cross-referenced") {
			continue
		}
		src := ev.GetSource()
		if src == nil || src.Issue == nil {
			continue
		}
		is := src.Issue
		repo := is.GetRepository().GetFullName()
		if repo == "" || is.GetNumber() <= 0 {
			continue
		}
		ref := parkRef{repo: repo, number: is.GetNumber(), isPR: is.IsPullRequest(), state: is.GetState(), closedAt: is.GetClosedAt().Time, body: is.GetBody()}
		if ref.isPR {
			ref.mergedAt = is.GetPullRequestLinks().GetMergedAt().Time
		}
		out[parkRefKey(repo, ref.number)] = ref
	}
	return out
}

// parkRelationWordRE matches the words that make a PR's mention of an issue
// a relation rather than a passing reference.
var parkRelationWordRE = regexp.MustCompile(`(?i)\b(?:refs?|references?|related\s+to|relates\s+to|part\s+of|addresses|implements|follow[- ]up\s+(?:to|of)|close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b`)

// parkRelationMaxRefs is the most references one relation clause may list
// and still count: a longer list is an intake table, not a relation.
const parkRelationMaxRefs = 10

// parkRefersTo reports whether a PR body refers to the issue with a relation
// word: the issue is among the references that follow the word up to the
// end of that sentence ("Refs #722 (…), #791 (…), #13."). A bare "#N"
// resolves to the PR's own repository, so "Refs other/repo#N" or a PR in
// another repository naming its own #N never reads as this issue.
func parkRefersTo(body, prRepo, issueRepo string, number int) bool {
	want := parkRefKey(issueRepo, number)
	for _, line := range strings.Split(body, "\n") {
		for _, loc := range parkRelationWordRE.FindAllStringIndex(line, -1) {
			clause := line[loc[1]:]
			if i := strings.Index(clause, ". "); i >= 0 {
				clause = clause[:i]
			}
			refs := parkNamedRefs(clause, prRepo, 0)
			if len(refs) > parkRelationMaxRefs {
				continue
			}
			for _, key := range refs {
				if key == want {
					return true
				}
			}
		}
	}
	return false
}

// parkChanges lists what moved after start, oldest first: named items that
// merged or closed, then (in time order with them) PRs referring to the
// issue that merged.
func parkChanges(refs map[string]parkRef, named []string, issueRepo string, number int, start time.Time) []parkChange {
	isNamed := map[string]bool{}
	for _, key := range named {
		isNamed[key] = true
	}
	render := func(ref parkRef) string {
		if strings.EqualFold(ref.repo, issueRepo) {
			return "#" + strconv.Itoa(ref.number)
		}
		return ref.repo + "#" + strconv.Itoa(ref.number)
	}
	var out []parkChange
	for key, ref := range refs {
		switch {
		case isNamed[key]:
			switch {
			case ref.isPR && ref.mergedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "merged", at: ref.mergedAt, named: true})
			case ref.isPR && ref.mergedAt.IsZero() && strings.EqualFold(ref.state, "closed") && ref.closedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "was closed without merging", at: ref.closedAt, named: true})
			case !ref.isPR && strings.EqualFold(ref.state, "closed") && ref.closedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "was closed", at: ref.closedAt, named: true})
			}
		case ref.isPR && ref.mergedAt.After(start) && parkRefersTo(ref.body, ref.repo, issueRepo, number):
			out = append(out, parkChange{ref: render(ref), verb: "merged", at: ref.mergedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].ref < out[j].ref
	})
	return out
}

// parkRecheckNote renders the note. It is deterministic for one set of
// changes, so an unchanged pass edits nothing.
func parkRecheckNote(label string, start time.Time, changes []parkChange) string {
	var b strings.Builder
	b.WriteString(parkRecheckMarker)
	fmt.Fprintf(&b, "\n**The reason this issue is parked may be out of date.** It has been parked for a maintainer (`%s`) since %s, and since then:\n\n", label, start.UTC().Format("2006-01-02"))
	for _, ch := range changes {
		why := "it refers to this issue"
		if ch.named {
			why = "the park reason names it"
		}
		fmt.Fprintf(&b, "- %s %s on %s (%s).\n", ch.ref, ch.verb, ch.at.UTC().Format("2006-01-02"), why)
	}
	b.WriteString("\nThe park reason was written before these changes. Re-read it against the current state before deciding. This note does not un-park the issue: it stays with a maintainer, and Hive updates this note if more of the referenced work moves.")
	return b.String()
}
