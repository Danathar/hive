package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// A trusted merger clearing the Sentinel label approves that head
// (sentinel.clear_counts_as_approval, off by default).
//
// A maintainer who reads a flagged diff and removes the alert label has made
// the trust decision, yet the hive kept asking for it a second time: a
// reviewer verdict of requires_human on that head (often "complete the
// sentinel trust-surface review") still waited for an Approve review or a
// dashboard Approve. With the setting on, the sweep reads who removed the
// label from GitHub's own label event and hands that to ClearApproval, which
// applies the trusted-merger test and records the person's approval of the
// head in the decision ledger. The sweep then notes it on its marker comment.
//
// Nothing here trusts text: the actor, the event id and its time come from
// the issue timeline; the head is the PR's current head, and the removal
// must have happened after that head reached the repository
// (sentinelRemovalAfterLatestPush, the rule sentinelClearedAfterLatestPush
// already applies). A clearance the sweep merely carries to a later head
// (sentinelClearanceCarriesOver) is a removal made before that head, so it
// records nothing.

// SentinelClearance is one removal of the alert label the sweep accepts as a
// person's clearance of HeadSHA.
type SentinelClearance struct {
	// Repo is owner/name.
	Repo    string
	Number  int
	HeadSHA string
	Label   string
	// Actor, EventID and At are the `unlabeled` timeline event's actor
	// login, id and server time.
	Actor   string
	EventID int64
	At      time.Time
	// PRAuthor opened the PR; CodeAuthor wrote its code (they differ only on
	// the hive's adoption of a contributor's PR). Neither can approve it.
	PRAuthor   string
	CodeAuthor string
}

// SentinelClearApproval is what ClearApproval decided about a clearance.
type SentinelClearApproval struct {
	// Approved is true when the actor's approval of the head stands in the
	// decision ledger; Recorded when this call is the one that wrote it.
	Approved bool
	Recorded bool
	// Reason says why nothing was recorded (not a trusted merger, the PR's
	// own author, …).
	Reason string
	// Err is a failed write to the ledger; the sweep tries again next pass.
	Err error
}

// sentinelClearApprovalNote closes the marker comment when a clearance was
// recorded as an approval; the arguments are the login that removed the
// label, the label name and the short head SHA.
const sentinelClearApprovalNote = "_@%s removed the `%s` label on head `%s`. On this hive a trusted merger's removal of that label also counts as their approval of that head (`sentinel.clear_counts_as_approval`), so Hive recorded it: a reviewer's `requires_human` on this head is answered. New commits need a new decision._"

// sentinelNoteWithApproval adds the approval sentence to a clearance note.
func sentinelNoteWithApproval(note, approval string) string {
	if approval == "" {
		return note
	}
	return note + "\n" + approval
}

// sentinelAwaitingClearPath is where the awaiting-clear map persists, next
// to the evaluated-heads file; "" (memory only) when that has no path.
func sentinelAwaitingClearPath(headsPath string) string {
	headsPath = strings.TrimSpace(headsPath)
	if headsPath == "" {
		return ""
	}
	return strings.TrimSuffix(headsPath, ".json") + "-awaiting-clear.json"
}

// forget drops key, writing the file only when something was dropped.
func (s *sentinelHeadsSeen) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[key]; !ok {
		return
	}
	delete(s.seen, key)
	s.saveLocked()
}

// reset drops every key, writing the file only when there was one.
func (s *sentinelHeadsSeen) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		return
	}
	s.seen = map[string]string{}
	s.saveLocked()
}

// sentinelClearApprovalPrepare runs at the start of every sweep entry point.
// With the setting off it empties the awaiting-clear map, so a label removed
// while the setting was off is never read as an approval after it is turned
// on again.
func (c *Client) sentinelClearApprovalPrepare(opts SentinelSweepOptions) {
	if opts.ClearApproval == nil {
		c.sentinelAwaitingClear.reset()
	}
}

// sentinelAwaitClear remembers that the sweep saw the blocking alert label
// on the PR at head. It is the only thing that makes a later removal worth a
// look: a PR whose head the sweep already evaluated costs no GitHub call
// otherwise. A no-op with the setting off.
func (c *Client) sentinelAwaitClear(opts SentinelSweepOptions, key, head string) {
	if opts.ClearApproval == nil || strings.TrimSpace(head) == "" {
		return
	}
	c.sentinelAwaitingClear.unchanged(key, head)
}

// sentinelClearApprovalDue reports whether a PR without the alert label was
// last seen wearing it at this same head, so the sweep must read it even
// though the head is not new.
func (c *Client) sentinelClearApprovalDue(opts SentinelSweepOptions, key, head string) bool {
	return opts.ClearApproval != nil && c.sentinelAwaitingClear.isSeen(key, head)
}

// sentinelClearApproval runs for a PR that no longer carries the alert
// label. When the sweep saw the label on this same head, the hive can name
// its own accounts, its own alert comment is on the PR, and the newest label
// event is a person's removal made after the head arrived, it asks
// opts.ClearApproval whether that person's removal is an approval. If so it
// appends sentinelClearApprovalNote to the hive's own marker comment (never
// a new comment) and returns the note, so a clearance record the sweep
// writes later in the same pass can carry it too.
//
// The awaiting entry is dropped once the question is settled either way, so
// each removal is looked at once; a read or write that fails keeps it and
// returns the error, and the next pass tries again. The ledger write is
// keyed by the label event, so a retry or a restart never records twice.
func (c *Client) sentinelClearApproval(ctx context.Context, opts SentinelSweepOptions, displayRepo, owner, repo string, pr *gh.PullRequest, label string) (string, error) {
	if opts.ClearApproval == nil {
		return "", nil
	}
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	key := sentinelKey(owner, repo, number)
	if !c.sentinelAwaitingClear.isSeen(key, head) {
		// Never seen labelled, or the head moved since: a new head needs a
		// new decision.
		c.sentinelAwaitingClear.forget(key)
		return "", nil
	}
	skip := func(reason, by string) (string, error) {
		c.sentinelAwaitingClear.forget(key)
		if c.logger != nil {
			c.logger.Info("sentinel label removed: cleared as before, no approval recorded", "repo", displayRepo, "pr", number, "head", head, "by", by, "reason", reason)
		}
		return "", nil
	}
	if !c.knowsOwnAccounts() {
		return skip("the hive cannot name its own accounts, so it cannot verify its alert comment", "")
	}
	own, err := c.newestOwnMarkerComment(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	if own == nil || !strings.Contains(own.GetBody(), sentinelAlertHeading) {
		return skip("the hive has no Sentinel alert comment on this PR", "")
	}
	h, actor, cleared, err := c.sentinelRemovalAfterLatestPush(ctx, owner, repo, number, head, label)
	if err != nil {
		return "", err
	}
	if !cleared {
		return skip("the newest label event is not a person's removal made after this head was pushed", strings.TrimSpace(h.lastActor.GetLogin()))
	}
	codeAuthor, _ := c.sentinelCodeAuthor(pr)
	out := opts.ClearApproval(SentinelClearance{
		Repo: owner + "/" + repo, Number: number, HeadSHA: head, Label: label,
		Actor: actor, EventID: h.lastID, At: h.lastAt,
		PRAuthor: pr.GetUser().GetLogin(), CodeAuthor: codeAuthor,
	})
	if out.Err != nil {
		return "", fmt.Errorf("recording sentinel clearance as an approval: %w", out.Err)
	}
	if !out.Approved {
		return skip(out.Reason, actor)
	}
	note := fmt.Sprintf(sentinelClearApprovalNote, actor, label, shortSHA(head))
	if !strings.Contains(own.GetBody(), note) {
		body := strings.TrimRight(own.GetBody(), "\n") + "\n" + note + "\n"
		if d := c.WritebackAllowed(displayRepo, c.sentinelWritebackItem(pr), writeback.KindSentinel, body); !d.Allowed {
			if c.logger != nil {
				c.logger.Info("sentinel clearance approval recorded; the write-back gate kept the note off the PR", "repo", displayRepo, "pr", number, "head", head, "by", actor, "reason", d.Reason)
			}
		} else {
			if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, own.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
				return "", fmt.Errorf("noting sentinel clearance approval: %w", err)
			}
			c.noteCommentEdited()
		}
	}
	c.sentinelAwaitingClear.forget(key)
	if c.logger != nil {
		c.logger.Info("sentinel label removed by a trusted merger: approval of this head recorded", "repo", displayRepo, "pr", number, "head", head, "by", actor, "new", out.Recorded)
	}
	return note, nil
}
