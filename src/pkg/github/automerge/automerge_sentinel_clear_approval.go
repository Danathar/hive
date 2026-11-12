package automerge

import (
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// trustedHumanLogin is the one trust test for a person whose own act answers
// a reviewer's requires_human: the login is not the hive App, not a bot, and
// the merger authorizer is configured and trusts it. A GitHub Approve review
// passes through it (standingTrustedHumanReviews) and so does a removal of
// the sentinel label (RecordSentinelClearApproval), so the two can never
// disagree about who counts. Fails closed on a nil authorizer.
func trustedHumanLogin(authz MergerAuthorizer, appBotLogin, login string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || strings.HasSuffix(login, "[bot]") {
		return false
	}
	if app := strings.TrimSpace(appBotLogin); app != "" && strings.EqualFold(login, app) {
		return false
	}
	return authz != nil && authz(login)
}

// mergerAuthorizer is the installed trusted-merger gate, nil when none is.
func (c *Engine) mergerAuthorizer() MergerAuthorizer {
	if c == nil {
		return nil
	}
	c.mergerAuthzMu.RLock()
	defer c.mergerAuthzMu.RUnlock()
	return c.mergerAuthz
}

// sentinelClearApprovalBody is the decision text recorded for a sentinel
// label removal that counts as an approval. It does not start with
// "approve (carried", which the hold guard reads as an approval nobody gave
// on that head.
const sentinelClearApprovalBody = "approve (removed the sentinel alert label on this head)"

// sentinelClearApprovalLookup is the optional store capability that finds a
// record by its source event whatever decision is newest on the head.
type sentinelClearApprovalLookup interface {
	Lookup(repo string, number int, source string, sourceID int64) (review.HumanChangeRequest, bool)
}

// RecordSentinelClearApproval decides whether a person's removal of the
// sentinel alert label (cl, read by the sentinel sweep from GitHub's label
// event) is also their approval of the head they cleared, and records it in
// store when it is (sentinel.clear_counts_as_approval).
//
// The actor must pass trustedHumanLogin, the test a GitHub Approve review
// must pass to answer requires_human, and must not be the PR's author or the
// author of the code it carries: nobody approves their own PR this way, the
// rule the merge queue applies to whoever queues a PR. Anyone else leaves the
// removal a Sentinel clearance and nothing more.
//
// The record is an approval-kind review.HumanChangeRequest on cl.HeadSHA,
// the kind a dashboard Approve writes, so every reader of the decision
// ledger treats the head as approved by that person and a new head as
// undecided. It is keyed by the label event and stamped with the event's
// time: recording the same removal again (a second sweep, a restart) changes
// nothing, and a change request the person left after removing the label
// stays the newer decision. Like any decision that is not a GitHub review it
// follows the PR over the hive's own base refresh (DecisionCarrier) and over
// nothing else.
func RecordSentinelClearApproval(store review.HumanChangeRequestStore, authz MergerAuthorizer, appBotLogin string, cl hgithub.SentinelClearance) hgithub.SentinelClearApproval {
	actor := strings.TrimSpace(cl.Actor)
	head := strings.TrimSpace(cl.HeadSHA)
	repo := strings.TrimSpace(cl.Repo)
	switch {
	case store == nil:
		return hgithub.SentinelClearApproval{Reason: "no decision ledger is configured"}
	case repo == "" || cl.Number <= 0 || head == "" || actor == "" || cl.EventID == 0 || cl.At.IsZero():
		return hgithub.SentinelClearApproval{Reason: "the label event is incomplete"}
	case strings.EqualFold(actor, strings.TrimSpace(cl.PRAuthor)) || strings.EqualFold(actor, strings.TrimSpace(cl.CodeAuthor)):
		return hgithub.SentinelClearApproval{Reason: "the PR's own author cannot approve it"}
	case !trustedHumanLogin(authz, appBotLogin, actor):
		return hgithub.SentinelClearApproval{Reason: "the remover is not a trusted merger"}
	}
	matches := func(r review.HumanChangeRequest) bool {
		return r.Source == review.HumanChangeRequestSourceSentinelClear && r.SourceID == cl.EventID && r.HeadSHA == head && r.IsApproval() && strings.EqualFold(strings.TrimSpace(r.Actor), actor)
	}
	if lookup, ok := store.(sentinelClearApprovalLookup); ok {
		if existing, found := lookup.Lookup(repo, cl.Number, review.HumanChangeRequestSourceSentinelClear, cl.EventID); found && matches(existing) {
			return hgithub.SentinelClearApproval{Approved: true}
		}
	} else if existing, found := store.For(repo, cl.Number, head); found && matches(existing) {
		return hgithub.SentinelClearApproval{Approved: true}
	}
	req := review.HumanChangeRequest{
		Repo: repo, Number: cl.Number, HeadSHA: head,
		Source: review.HumanChangeRequestSourceSentinelClear, SourceID: cl.EventID,
		Actor: actor, Body: sentinelClearApprovalBody, Kind: review.HumanChangeRequestKindApprove,
		RecordedAt: cl.At,
	}
	if err := store.Record(req); err != nil {
		return hgithub.SentinelClearApproval{Err: err}
	}
	return hgithub.SentinelClearApproval{Approved: true, Recorded: true}
}

// sentinelClearCountsAsApproval reports the live
// sentinel.clear_counts_as_approval switch (StageLabelPolicy); false when
// no stage policy is wired.
func (c *Engine) sentinelClearCountsAsApproval() bool {
	return c.stagePolicy().SentinelClearApproves
}
