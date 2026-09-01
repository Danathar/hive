# Reviewer lane

The reviewer lane is an opt-in L5/L6 worker for agent-authored pull requests that have exhausted Hive's deterministic fix-loop budget and carry `needs-human`. It is deliberately separate from the structured review swarm: `review.require_approval` votes on healthy PRs before merge, while the reviewer lane adjudicates already-escalated PRs after ordinary repair automation has stopped. A queued PR may now be green when a base regression or infrastructure failure clears; it remains queued so the reviewer can verify and de-escalate it deliberately.

## Enabling it

The built-in L5 and L6 ACMM packs include a `reviewer` agent, but every governor-mode cadence is `paused`. The lane also has an independent config gate, which defaults off:

```yaml
escalation:
  reviewer:
    enabled: true
    agent: reviewer       # optional; this is the default
    max_per_cycle: 3      # optional; this is the default
```

Both gates must be opened: set `enabled: true`, then give the reviewer a conservative cadence in the Governor cadence editor. The configured target must also declare `role: reviewer`; naming an ordinary coding agent does not broaden its queue. Below L5 the lane fails closed even if stale config says enabled. Turning off the config gate immediately stops escalated-queue injection; pausing the agent or its cadence provides the usual operational stop.

## Queue and authority

The governor writes a dedicated `reviewer_queue` alongside the ordinary failure snapshot. Membership requires both an authoritative `needs-human`/new-escalation signal and an author matching `project.ai_author` or a bot; it does not require the PR to remain red. The reviewer never receives human-authored PRs and is told not to rebuild the queue with GitHub searches. One kick contains at most `max_per_cycle` entries.

For each assigned PR, the reviewer must verify that the change is still wanted and is lossless against the current base (including diff and test-count parity), then choose one outcome:

- **Repair:** fix and test the existing branch, push it, and remove `needs-human`.
- **De-escalate:** for a proven base-branch regression or infrastructure failure, update/rebase the existing branch and return it to automation.
- **Recommend-close / close:** duplicates, superseded changes, and lossy branches receive a rationale. L5 leaves the final close to an operator; L6 may close it. The reviewer never merges.

No outcome may open a replacement PR. Repair and de-escalation return a green branch to the existing merge lane rather than granting the reviewer a new merge path.

## One-pass and audit invariant

Every adjudication must do all three:

1. submit a comment review through `hive-review`, producing the durable `agent_pr_reviewed` audit/activity event;
2. create an advisory bead with the outcome and rationale, which feeds the advisory digest;
3. apply `hive/reviewer-pass` and never remove it.

The scheduler excludes every PR carrying `hive/reviewer-pass`, even if `needs-human` returns later. If a passed PR is observed red, deterministic escalation code posts the latest evidence, restores `needs-human`, and records the terminal handoff in the escalation ledger. From then on only a true human removes the label. This makes the automation ladder finite:

```text
ordinary fix attempts -> machinery amnesty/re-engagement budget -> one reviewer pass -> true human
```

The pass label is intentionally durable across restarts, green cycles, and future regressions on the same PR. Removing it manually would erase the no-ping-pong invariant and is unsupported.

## Limitations

The reviewer is policy-bounded to closing only at L6; it uses the same `ISSUES_AND_PRS` GitHub capability tier at L5 and L6 so it can push repairs, label the adjudication, and post notes. The deterministic safeguards are queue provenance, the independent L5+ config gate, the per-kick cap, persistent one-pass label, and terminal re-escalation. Operators should keep the lane paused until they trust its model and policy, and should review its `agent_pr_reviewed` events and advisory beads after initial runs.
