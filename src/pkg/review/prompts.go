package review

import (
	"fmt"
	"strings"
)

type PullRequest struct {
	Repo    string
	Number  int
	Title   string
	Author  string
	HeadSHA string
	URL     string
	Lane    string
	// MergeBase is the commit the reviewer should ground its reading in. When
	// set, the prompt instructs the reviewer to read the repository at this
	// commit rather than reasoning from the diff alone — see groundingSection
	// for the measurement that makes this the load-bearing field.
	MergeBase string
}

// Grounding thresholds, named so the numbers never appear bare in the prompt.
//
// The tool-call budget mirrors the measured experiment: the grounded arm used a
// mean of 10.6 tool calls under a cap of 25, so 25 is a ceiling that did not
// bind in practice rather than a limit tuned to force brevity. It exists to
// bound worst-case cost, not to shape behavior.
const (
	// GroundingToolCallBudget caps repository investigation per perspective.
	GroundingToolCallBudget = 25
)

// groundingSection instructs the reviewer to read the actual tree, and it is
// the single most important paragraph in this file.
//
// An experiment (2026-08-28) scored three review arms against six merged hive
// PRs with pre-registered ground truth, blind scoring, and counted false
// positives:
//
//	Arm A  diff + the author's own PR body     0/6 hits (0%)   3.3 FP/PR
//	Arm B  diff + linked issue, cleared        1/6 hits (17%)  3.6 FP/PR
//	Arm C  cleared + repo at merge-base        4/6 hits (67%)  1.4 FP/PR
//
// Two findings shape this text:
//
//  1. REPO ACCESS IS THE ACTIVE INGREDIENT. B→C moved hit rate 17%→67% while
//     CUTTING false positives 61%. Grounding both finds real defects and
//     suppresses invented ones — the diff-only arms filled the gap with
//     confidently-argued claims the code did not support. Without this section
//     the review swarm IS Arm B: ~3.6 false positives per PR, roughly nine
//     non-issues per real finding.
//
//  2. THE AUTHOR'S RATIONALE ACTIVELY HURT. Arm A, the only arm shown the PR
//     body, was the worst. This prompt therefore never includes the PR body,
//     and PullRequest deliberately has no Body field so it cannot. That
//     absence is pinned by TestPromptExcludesAuthorRationale.
//
// Caveat, recorded because it bounds how much authority a verdict should carry:
// n=6, one run per cell. Directional, not definitive.
func groundingSection(pr PullRequest) string {
	var b strings.Builder
	b.WriteString("\nGROUNDING — read the code, do not infer it.\n")
	if pr.MergeBase != "" {
		fmt.Fprintf(&b, "A checkout of this repository at merge-base %s is available to you. ", pr.MergeBase)
	} else {
		b.WriteString("A checkout of this repository is available to you. ")
	}
	b.WriteString("Open the files the diff touches and the files that CALL them.\n")
	b.WriteString("- Every finding MUST cite concrete file:line evidence you actually read. A finding you cannot cite is the signature of a false positive — omit it.\n")
	b.WriteString("- Before asserting a defect, check the surrounding code for the guard, early return, or caller that would already prevent it.\n")
	b.WriteString("- Prefer one verified finding over three plausible ones. Measured: reviewers that read the tree found 4x more real defects AND made 61% fewer false claims.\n")
	b.WriteString("- If you cannot verify a concern, either state it as an explicit open question or leave it out. Do not assert it as a defect.\n")
	fmt.Fprintf(&b, "- Budget: up to %d investigation tool calls for this perspective.\n", GroundingToolCallBudget)
	return b.String()
}

// BuildPerspectivePrompt builds the per-PR review kick without the publish
// step, preserving the original behavior for callers that have not opted in.
func BuildPerspectivePrompt(p Perspective, pr PullRequest) string {
	return BuildPerspectivePromptOpts(p, pr, false)
}

// BuildPerspectivePromptOpts builds the kick and, when postComments is set,
// appends the instruction to publish the verdict on the PR through the
// `hive-review` relay.
//
// Without that step a reviewer's product is a JSON aggregate consumed only by
// the merge gate. On a hive that does not auto-merge, nothing consumes it: the
// review is performed, serialized, and discarded. Publishing is what turns it
// into help for the human who actually has to decide.
//
// Deliberately comment-only. approve/request_changes set a review state that
// influences merge, and that authority is not this agent's to exercise — the
// relay gates those separately, and the whole point here is to inform a human
// rather than to gate them.
func BuildPerspectivePromptOpts(p Perspective, pr PullRequest, postComments bool) string {
	focus := map[Perspective]string{
		PerspectiveCorrectness:     "correctness, regressions, edge cases, data races, and test adequacy",
		PerspectiveSecurity:        "exploitable vulnerabilities, unsafe permissions, injection, secrets, and trust-boundary regressions",
		PerspectiveIntentAlignment: "whether the diff solves the linked issue without unrelated scope creep",
		PerspectiveStyle:           "maintainability, conventions, readability, and repository idioms",
		PerspectiveDocsCurrency:    "documentation, examples, generated docs, and operator-facing text that must change with behavior",
	}[p]
	if focus == "" {
		focus = "the named review perspective"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[review-perspective:%s]\n", p)
	fmt.Fprintf(&b, "Review PR %s#%d", pr.Repo, pr.Number)
	if pr.Title != "" {
		fmt.Fprintf(&b, " — %s", pr.Title)
	}
	b.WriteString(".\n")
	if pr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", pr.URL)
	}
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", pr.HeadSHA)
	}
	if pr.Author != "" {
		fmt.Fprintf(&b, "Author: @%s\n", strings.TrimPrefix(pr.Author, "@"))
	}
	fmt.Fprintf(&b, "Focus ONLY on %s. Do not duplicate other perspectives unless the issue is severe.\n", focus)
	b.WriteString(groundingSection(pr))
	b.WriteString("\nReturn exactly one JSON object: the standard outputschema AgentReport fields plus perspective, verdict, repo, number, and head_sha.\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	b.WriteString("Allowed verdicts: approve, changes_requested, requires_human, reject. Finding severities: info, low, medium, high, critical.\n")
	b.WriteString("Use approve only when this perspective finds no blocker. Use changes_requested for agent-fixable issues. Use requires_human for ambiguous/high-risk judgment. Use reject for fundamentally unsuitable or harmful PRs.\n")
	if postComments {
		b.WriteString(buildPublishInstruction(pr))
	}
	return b.String()
}

// buildPublishInstruction is the publish half of the kick: how to say what you
// found, and — more importantly — when to say nothing.
func buildPublishInstruction(pr PullRequest) string {
	var b strings.Builder
	b.WriteString("\nPUBLISH YOUR VERDICT.\n")
	b.WriteString("After you produce the JSON, post your findings as a PR comment so a human sees them:\n")
	fmt.Fprintf(&b, "  hive-review %d --repo %s --comment --body-file <file>\n", pr.Number, pr.Repo)
	b.WriteString("Use hive-review, never `gh pr review` — it is submitted with the App token and recorded on the audit trail.\n")
	b.WriteString("Only --comment. Do NOT approve, request changes, merge, close, or label; a human decides those.\n")
	b.WriteString("Every claim in the comment must cite file:line you actually read. A finding you cannot point at is a false positive, and it now costs a contributor their time to refute.\n")
	b.WriteString("Say nothing rather than pad. Do NOT post a comment that is only nits, only praise, or a restatement of the diff. If this perspective found nothing a human needs, skip the comment entirely and just return the JSON.\n")
	b.WriteString("If the PR body claims behavior the diff does not implement, say so with file:line — that gap is one of the most useful things you can report.\n")
	b.WriteString("Be brief and specific. One comment, at most a few findings, worst first.\n")
	return b.String()
}
