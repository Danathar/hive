package github

import (
	"strings"

	"github.com/hivecommons/hive/pkg/decisionpolicy"
)

// standingApprovalAuditPolicy is the "policy" value on the
// agent_issue_decision_resolved audit line when an owner's standing
// approval, not the resolve policy, took the decision.
const standingApprovalAuditPolicy = "standing_approval"

// SetStandingApprovalFunc installs the project.standing_approvals lookup.
// The hive passes config's StandingApprovalCovers, so an entry edited in
// hive.yaml is in force on the next relay request; nil clears it, and then
// no request is downgraded on a standing approval.
func (c *Client) SetStandingApprovalFunc(fn func(id, agent, repo string) bool) {
	if c == nil {
		return
	}
	c.standingApprovalMu.Lock()
	defer c.standingApprovalMu.Unlock()
	c.standingApprovalCovers = fn
}

func (c *Client) standingApprovalCoversRequest(id, agent, repo string) bool {
	if c == nil {
		return false
	}
	c.standingApprovalMu.RLock()
	covers := c.standingApprovalCovers
	c.standingApprovalMu.RUnlock()
	return covers != nil && covers(id, agent, repo)
}

// standingApprovalDowngrade decides whether the standing approval a
// --needs-decision request names lets it skip the park. It applies only
// when the entry exists and covers this agent and repo, and the request's
// `Risk:` line is a governance question outside the floor topics
// (decisionpolicy.StandingApprovalRefusal). It is independent of
// agent_issue_decision_policy: an owner who lists an entry has decided that
// question in advance, whichever policy handles the rest.
//
// The caller has already checked NeedsDecision, OwnerStep and that a hive
// agent sent the request. A request that names no standing approval returns
// the zero value.
func (c *Client) standingApprovalDowngrade(req IssueRequest, title, body string) issueDecisionDowngrade {
	id := strings.TrimSpace(req.StandingApproval)
	if id == "" {
		return issueDecisionDowngrade{}
	}
	if !c.standingApprovalCoversRequest(id, req.Agent, req.Repo) {
		return issueDecisionDowngrade{Refused: "standing approval `" + id + "` does not exist or does not cover agent " + req.Agent + " in " + req.Repo}
	}
	if reason := decisionpolicy.StandingApprovalRefusal(body); reason != "" {
		return issueDecisionDowngrade{Refused: reason}
	}
	question := decisionQuestionSummary(title, body)
	return issueDecisionDowngrade{
		Active:           true,
		Question:         question,
		StandingApproval: id,
		Note: "Hive did not park this: the owner approved this kind of change in advance in standing approval `" + id +
			"` — " + question + ". Owner may override with `/hive hold` or `needs-human`.",
	}
}

// recordStandingApprovalResult reports on resp what became of a requested
// standing approval: the id when it took the decision, otherwise why not.
func recordStandingApprovalResult(resp *IssueResponse, req IssueRequest, d issueDecisionDowngrade) {
	id := strings.TrimSpace(req.StandingApproval)
	if id == "" {
		return
	}
	if d.Active && d.StandingApproval != "" {
		resp.StandingApproval = d.StandingApproval
		return
	}
	resp.StandingApprovalRefused = d.Refused
	if resp.StandingApprovalRefused == "" {
		resp.StandingApprovalRefused = "a standing approval applies only to an issue or comment request that also sets needs_decision, from a hive agent, with no owner step"
	}
}
