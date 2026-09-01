# Formal model: escalation + reviewer-lane lifecycle

A Spin (Promela) model of the fix-loop escalation machinery and the reviewer
adjudication lane, at the protocol level: one hive-authored PR's lifecycle as
nine interacting processes. It verifies the invariants the code comments
promise — and pins down, with minimized counterexamples, the places where the
shipped machinery does not deliver them.

The model targets the **post-#5485** machinery (v5 with the reviewer lane,
#5480, merged; machinery-generation amnesty, #5471).

Run everything:

```console
$ bash run.sh        # requires spin + a C compiler; ~1 minute
```

`run.sh` runs every property exhaustively and compares each verdict against
its expected result. Properties that must hold must pass; properties with a
**documented counterexample** (the design gaps below) must still *fail* — if
one stops failing, the code or the model changed and a human must re-check.
Either divergence exits nonzero. CI runs this via
`.github/workflows/formal-verify.yml` (reporting-only, not a required check).

## Model ↔ code mapping

| Promela | Go |
|---|---|
| `Sweeper` proctype | `runEscalationSweep` (`cmd/hive/main.go`) driving `Store.Sweep` (`pkg/escalation/escalation.go`) once per governor tick |
| `Reaper` proctype | `reapStuckRedPRs` (`cmd/hive/main.go`) → `Store.TryReEngage` |
| `Agent` proctype | the FIX-BEFORE-NEW fix lane (`pkg/scheduler/scheduler.go:834-893`): red, non-escalated PRs are routed back to their author agent, which pushes another attempt |
| `Reviewer` proctype | the reviewer lane (`pkg/scheduler/reviewer_lane.go`): adjudicates escalated rows from `ci-failing.json`, excluding rows labeled `reviewer-passed`; REPAIR and DE-ESCALATE are collapsed into one branch (both = label swap + push); RECOMMEND-CLOSE closes at ACMM ≥ 6 (`-DACMM6`), else comments and stands down |
| `Human` proctype | the operator watching the `needs-human` label queue; eventually takes ownership or closes. **The queue is the label** — a PR without `needs-human` is invisible to the human |
| `CI` proctype | per-SHA check verdict: each pushed SHA resolves nondeterministically red or green, once |
| `Merger` proctype | the auto-merge sweep: a green open PR is eventually merged |
| `Timer` proctype | wall clock for `Store.StaleRed`: the tracked `CurRedSHA`, unchanged, eventually exceeds `RedPRStaleAfter` |
| `GenBumper` proctype | `MachineryVersion` bumps (a new fix-dispatch generation ships) |
| `MergeWatcher` proctype (`-DWATCHER`) | the merge-request watcher's terminal re-engage path (`pkg/github/merge_request_watcher.go:276`) |
| `entryExists`, `attempts`, `escalated`, `curRedSha`, `reeng`, `entryGen` | `escalation.Entry`: existence in the ledger, `len(RedSHAs)`, `Escalated`, `CurRedSHA`, `ReEngagements`, `Machinery` |
| `headCounted` | `containsSHA(e.RedSHAs, o.HeadSHA)` for the current head (sound: SHAs are never reused, so only the current head's membership is ever queried) |
| `stale` | `now − e.FirstRedAt ≥ RedPRStaleAfter` (`Store.StaleRed`) |
| `escalatedView` | the `escalatedPRs` map returned by `runEscalationSweep` and consumed same-tick by `reapStuckRedPRs` and `writeMergeEligible` (`ci-failing.json` `row.Escalated`); refreshed at the end of each sweep pass |
| `needsHuman`, `reviewerPassed` | the `needs-human` / `reviewer-passed` labels on the forge |
| `amnesty_check()` inline | the machinery-amnesty block shared by `Store.Sweep` (escalation.go:181-186) and `Store.TryReEngage` (escalation.go:372-377) |
| `THRESHOLD=3`, `MAX_REENG=6` | `DefaultThreshold`, `MaxReEngagements` (actual production constants) |
| `NSHA=4`, `GEN_MAX=3` | finite bounds: distinct head SHAs per PR, machinery generations |

Faithfully carried quirks: `Store.TryReEngage` creates missing entries as
`&Entry{}` (Machinery **0**, → immediate no-op amnesty), while `Store.Sweep`
creates them with `Machinery: MachineryVersion`; `Sweep`'s green branch is
`!o.Red`, which **includes CI-pending observations** (see gap G2); the
reviewer edits labels only — the ledger's `Escalated` flag is untouched (gap
G1); `TryReEngage` never consults `Escalated` (gap G3).

## What is abstracted away

- **One PR.** All properties are per-PR; the ledger's cross-PR pruning
  (`seen` map) reduces to "clear on close/merge". `maxTrackedSHAs=20` is
  unreachable at `NSHA=4`.
- **SHAs are ordinals.** Pushes mint strictly increasing SHA ids; a SHA's CI
  verdict resolves once (a flake re-run would be a new push). Rebases and
  repairs are pushes.
- **Time is nondeterminism.** Sweep cadence, `RedPRStaleAfter`, and CI
  latency are unordered events; every interleaving is explored, which is
  strictly more behavior than any real timing.
- **Evidence/comments/ntfy** (excerpts, `CommentBody`, notifications) carry
  no protocol state and are dropped. `MarkEscalated`'s comment-retry loop is
  collapsed into the escalation step.
- **ObserveRed** (`recordRedStaleness`) is folded into `Timer` + the sweep's
  SHA sync; its entry-creation path is represented by `TryReEngage`'s
  (both create `&Entry{}` with Machinery 0).
- **ACMM** is fixed at 5 (lane active) — below 5 the reviewer is a no-op and
  every property reduces to the pre-#5480 machinery; `-DACMM6` models
  close authority.
- Property monitors are compiled in per run (`-DMON_*`) so each exhaustive
  search carries only the instrumentation it needs; monitor counters saturate
  at the bounds they are compared against.

## Verification results

Spin 6.5.2, exhaustive (`./pan -m500000`, DFS + COLLAPSE; liveness with weak
fairness `-a -f`). Every state count is a completed full search.

| # | Property | Run(s) | Result | States |
|---|---|---|---|---|
| P1 | Amnesty is one-shot per generation (per entry lifetime) | `p1_p2_amnesty` (assert in `amnesty_check`) | **holds** | 5,598,270 |
| P2 | No instant re-escalation after amnesty: never in the amnesty pass itself, and only with ≥ `THRESHOLD−1` genuinely new red SHAs or a full fresh re-engage budget | `p1_p2_amnesty` (asserts P2a/P2b) | **holds** | 5,598,270 |
| P3 | Re-engage budget: `ReEngagements ≤ 6` always; per unchanged red SHA, total ≤ (1 + amnesties) × 6 — i.e. ≤ 12 across one generation bump, ≤ 18 across the 3 modeled generations | `p3_budget_asserts`, `p3_cap_ltl`, `p3_total_ltl` | **holds** | 10,374,648 / 2,100,236 / 10,374,648 |
| P4a | Once `reviewer-passed`, the reviewer never adjudicates the PR again | `p4a_p6_safety` (assert) | **holds** | 2,100,236 |
| P4b | …and such a PR, if (still) escalated, eventually reaches the human queue | `p4b_handoff` (LTL, weak fairness) | **VIOLATED — gap G1** | cycle at 7,207 |
| P5 | Every escalated PR eventually terminal (merged / closed / human-owned) | `p5_termination` (LTL, weak fairness) | **VIOLATED — gap G1** | cycle at 5,169 |
| P5′ | P5 and P4b under the hypothetical ledger-sync fix | `patch_p5_termination`, `patch_p4b_handoff` (`-DPATCH_REVIEWER`) | **hold** | 2,104,638 / 2,098,622 |
| P6 | Escalated entries are invisible to the reaper and to FIX-BEFORE-NEW | `p4a_p6_safety`, `p6_watcher_safety` (asserts) | **holds** | 2,100,236 / 20,749,779 |
| W1 | "One reviewer pass per PR, ever" for RECOMMEND-CLOSE below ACMM 6 | `w_onepass_acmm5` / `w_onepass_acmm6` | **witness found** at ACMM 5 (holds at ACMM 6, 2,140,106 states) — gap G4 | 4,113 |
| W2 | Ledger history survives until green | `w_pending_wipe` | **witness found** — gap G2 | 4,636 |
| W3 | The merge watcher never re-engages an escalated PR | `w_watcher_reengage` (`-DWATCHER`) | **witness found** — gap G3 | 207,685 |

## Design gaps found

### G1 — a reviewer pass on a PR that stays red orphans it (REAL, liveness)

`Entry.Escalated` is cleared only by: the PR going green (entry deleted), the
PR leaving the open set, or machinery amnesty. The reviewer's verdicts edit
**labels only**. Minimized counterexample (`spin -t -p` on `p5_termination`):

1. Three distinct red SHAs → `Sweep` escalates: `Escalated=true`,
   `needs-human` added.
2. The reviewer REPAIRs (or DE-ESCALATEs): removes `needs-human`, adds
   `reviewer-passed`, pushes a fix SHA.
3. The fix resolves **red**.
4. Now: the ledger still says `Escalated` → the row is excluded from
   FIX-BEFORE-NEW and the reaper; `reviewer-passed` excludes it from the
   reviewer lane; `NewlyEscala` requires `!e.Escalated`, so the `needs-human`
   label (and ntfy) can never fire again — the human queue never sees it.
   The PR sits red and open forever, owned by nobody.

The escalation comment's promise — "Remove the `needs-human` label … to
return the PR to the automated fix lane" — is broken by the same root cause:
the lane consults the ledger flag, not the label, so a human removing the
label (without the PR reaching green) also does not re-enable the fix lane.

Two nondeterministic rescues exist, and neither is a guarantee: a later
`MachineryVersion` bump (amnesty un-escalates the ledger), or gap G2's
pending-wipe happening to fire during the fix's CI window. The model shows
runs where neither occurs.

`-DPATCH_REVIEWER` verifies the obvious fix shape: when the reviewer removes
`needs-human`, also reset the ledger (`Escalated=false`, `RedSHAs=nil` — the
same reset amnesty performs). With that, P4b and P5 hold: a still-red repair
re-enters the fix lane, re-escalates on fresh evidence, `NewlyEscala`
re-fires, and the `reviewer-passed` exclusion hands it to the human.

### G2 — a CI-pending observation wipes the attempt ledger (REAL, counting)

`Store.Sweep` branches on `!o.Red`, and `runEscalationSweep` computes
`Red: pr.CIStatus == "failure"` — so a **pending** observation (every fresh
push has a pending window) takes the "went green" branch and **deletes the
entry**, discarding the distinct-SHA attempt count. A PR that always happens
to be enumerated mid-window restarts its count every time and can evade the
threshold indefinitely (witness `w_pending_wipe`). In practice sweeps usually
catch settled states, which is why escalation still fires — but the breaker
is probabilistic where the code intends it to be deterministic. (This wipe is
also what accidentally rescues some G1 orphans.) A fix would treat pending as
a no-op observation: only green clears, only red counts.

### G3 — the merge watcher re-engages escalated PRs (minor)

`Store.TryReEngage` never checks `Entry.Escalated`, and the merge-request
watcher's terminal path (`merge_request_watcher.go:276`) calls it without
consulting the escalated set (unlike `reapStuckRedPRs`, which does). A merge
request filed before escalation that exhausts its attempts on a red required
check therefore burns re-engagement budget on a `needs-human` PR and logs
"re-engaged fix loop" for a PR the fix loop is standing down from. Benign in
effect — the dispatch surfaces (FIX-BEFORE-NEW, reaper) still exclude
escalated rows, and P3/P6 hold in the `-DWATCHER` build — but the counter and
the log line lie, and a budget burned this way can later trigger an
exhaustion-based re-escalation path prematurely after amnesty.

### G4 — RECOMMEND-CLOSE below ACMM 6 re-adjudicates every kick (minor)

The work-list builder's "one reviewer pass per PR, ever" holds only for
REPAIR/DE-ESCALATE, because only those add `reviewer-passed`. Below ACMM 6, a
RECOMMEND-CLOSE verdict leaves the row fully adjudicable, so the reviewer
re-adjudicates (and re-comments) it on every kick until a human acts
(witness `w_onepass_acmm5`; at ACMM 6 the close is immediate and the
invariant holds). A fix would label or otherwise mark recommend-closed rows.

## Reproducing a counterexample

```console
$ spin -a escalation.pml && gcc -O2 -DNFAIR=12 -o pan pan.c
$ ./pan -a -f -i -N p5_term        # -i: iteratively shorten the trail
$ spin -t -p -g escalation.pml     # replay with state annotations
```
