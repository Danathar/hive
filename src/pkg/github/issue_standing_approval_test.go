package github

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// standingApprovalCovers stands in for config.StandingApprovalCovers: one
// entry, "advisory-ci-checks", for the quality lane in o/r only.
func standingApprovalCovers(id, agent, repo string) bool {
	return id == "advisory-ci-checks" && agent == "quality" && repo == "o/r"
}

// runStandingApprovalCreate files req through the relay under policy and
// returns the create payload and the request's result.
func runStandingApprovalCreate(t *testing.T, policy string, req IssueRequest) (issueCreatePayload, IssueResponse) {
	t.Helper()
	var mu sync.Mutex
	var got []issueCreatePayload
	srv := newIssueCreateLabelServer(t, &mu, &got)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAgentIssueDecisionPolicy(policy)
	c.SetStandingApprovalFunc(standingApprovalCovers)
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, req)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 issue create, got %d", len(got))
	}
	return got[0], readIssueResultFile(t, reqPath)
}

const gateProposalBody = "Add an advisory mutation job.\n\nRisk: governance\n"

func gateProposalRequest() IssueRequest {
	return IssueRequest{
		Repo: "o/r", Title: "[quality] Gate proposal: mutation check", Body: gateProposalBody,
		Labels: []string{"agent/quality"}, Agent: "quality", NeedsDecision: true,
		StandingApproval: "advisory-ci-checks",
	}
}

// A covered governance park is filed actionable, with the approval recorded
// in the body and on the result. It holds under `escalate` too: the owner
// decided this question in advance.
func TestStandingApproval_CoveredGovernanceParkIsFiledActionable(t *testing.T) {
	for _, policy := range []string{"resolve", "escalate"} {
		payload, resp := runStandingApprovalCreate(t, policy, gateProposalRequest())
		if hasLabelFold(payload.Labels, NeedsDecisionStageLabel) || !hasLabelFold(payload.Labels, "stage/010-actionable") {
			t.Fatalf("%s: labels = %v, want actionable and not parked", policy, payload.Labels)
		}
		if !strings.Contains(payload.Body, "Decision taken by hive: Hive did not park this: the owner approved this kind of change in advance in standing approval `advisory-ci-checks`") {
			t.Fatalf("%s: body does not record the standing approval:\n%s", policy, payload.Body)
		}
		if strings.Contains(payload.Body, unparkOwnerStepMarker) {
			t.Fatalf("%s: a downgraded issue must not carry the owner-step marker", policy)
		}
		if resp.StandingApproval != "advisory-ci-checks" || resp.StandingApprovalRefused != "" {
			t.Fatalf("%s: result = %+v, want the applied id and no refusal", policy, resp)
		}
	}
}

// Every way the approval can fail to apply parks the issue as before and
// says why on the result. Repo scoping is config.StandingApprovalCovers's
// job and is tested there; this test client serves one repo.
func TestStandingApproval_RefusedRequestsStillPark(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*IssueRequest)
		reason string
	}{
		{"unknown id", func(r *IssueRequest) { r.StandingApproval = "no-such-entry" }, "does not exist or does not cover"},
		{"other lane", func(r *IssueRequest) { r.Agent = "scanner"; r.Labels = []string{"agent/scanner"} }, "does not exist or does not cover"},
		{"no risk line", func(r *IssueRequest) { r.Body = "Add an advisory mutation job." }, "no `Risk:` line"},
		{"security risk", func(r *IssueRequest) { r.Body = "Risk: security posture\n" }, "security posture"},
		{"floor topic in a governance line", func(r *IssueRequest) { r.Body = "Risk: governance (the live ruleset)\n" }, "ruleset"},
		{"owner step", func(r *IssueRequest) {
			r.OwnerStep = &IssueOwnerStep{Kind: "release", Tag: "v1.0.0", Target: strings.Repeat("a", 40)}
		}, "no owner step"},
	}
	for _, tc := range cases {
		req := gateProposalRequest()
		tc.mutate(&req)
		payload, resp := runStandingApprovalCreate(t, "escalate", req)
		if !hasLabelFold(payload.Labels, NeedsDecisionStageLabel) {
			t.Errorf("%s: labels = %v, want the park kept", tc.name, payload.Labels)
		}
		if resp.StandingApproval != "" || !strings.Contains(resp.StandingApprovalRefused, tc.reason) {
			t.Errorf("%s: result = %+v, want a refusal mentioning %q", tc.name, resp, tc.reason)
		}
	}
}

// A refused standing approval falls back to the resolve policy, so a plain
// engineering question is still downgraded the ordinary way.
func TestStandingApproval_RefusalFallsBackToResolvePolicy(t *testing.T) {
	req := gateProposalRequest()
	req.StandingApproval = "no-such-entry"
	req.Body = "Question: which ADR gets renumbered / use a v6-only ADR block"
	payload, resp := runStandingApprovalCreate(t, "resolve", req)
	if !hasLabelFold(payload.Labels, "stage/010-actionable") {
		t.Fatalf("labels = %v, want the resolve downgrade", payload.Labels)
	}
	if !strings.Contains(payload.Body, "engineering decision taken by hive") {
		t.Fatalf("body = %q, want the engineering-decision line", payload.Body)
	}
	if resp.StandingApprovalRefused == "" {
		t.Fatalf("result = %+v, want the refusal reported even though the resolve policy downgraded it", resp)
	}
}

// Without needs_decision there is nothing to downgrade: the issue is filed
// exactly as asked and the result says the flag did nothing.
func TestStandingApproval_WithoutNeedsDecisionIsReportedNotApplied(t *testing.T) {
	req := gateProposalRequest()
	req.NeedsDecision = false
	payload, resp := runStandingApprovalCreate(t, "resolve", req)
	if hasLabelFold(payload.Labels, NeedsDecisionStageLabel) || hasLabelFold(payload.Labels, "stage/010-actionable") {
		t.Fatalf("labels = %v, want the agent's labels untouched", payload.Labels)
	}
	if !strings.Contains(resp.StandingApprovalRefused, "needs_decision") {
		t.Fatalf("result = %+v, want a refusal naming needs_decision", resp)
	}
}

// `comment --needs-decision --standing-approval` on an existing issue posts
// the comment with the approval recorded and does not park.
func TestStandingApproval_CommentIsNotParked(t *testing.T) {
	writes, bodies, resp := runNeedsDecisionCommentCapture(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 31, Agent: "quality", NeedsDecision: true,
		StandingApproval: "advisory-ci-checks", Body: gateProposalBody,
	}, false, func(c *Client) { c.SetStandingApprovalFunc(standingApprovalCovers) })
	if got := strings.Join(writes, ","); got != "comment" {
		t.Fatalf("writes = %q, want only the comment", got)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "standing approval `advisory-ci-checks`") || strings.Contains(bodies[0], unparkOwnerStepMarker) {
		t.Fatalf("bodies = %v, want the approval recorded and no owner-step marker", bodies)
	}
	if resp.StandingApproval != "advisory-ci-checks" {
		t.Fatalf("result = %+v, want the applied id", resp)
	}
}

// With no lookup installed (no standing_approvals configured) nothing is
// downgraded on a standing approval.
func TestStandingApproval_NoLookupParks(t *testing.T) {
	writes, _, resp := runNeedsDecisionCommentCapture(t, nil, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 32, Agent: "quality", NeedsDecision: true,
		StandingApproval: "advisory-ci-checks", Body: gateProposalBody,
	}, false, nil)
	if got := strings.Join(writes, ","); got != "label:"+NeedsDecisionStageLabel+",comment" {
		t.Fatalf("writes = %q, want park then comment", got)
	}
	if resp.StandingApprovalRefused == "" {
		t.Fatalf("result = %+v, want a refusal", resp)
	}
}
