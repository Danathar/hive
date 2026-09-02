#!/usr/bin/env bash
# Exhaustive Spin verification of escalation.pml — every property, every run.
#
# Each row of the matrix below carries an EXPECTED verdict:
#   pass — the property must hold (0 errors); a violation fails this script.
#   fail — a KNOWN counterexample exists (documented design gap or witness,
#          see README.md). The run must still find it: if a "fail" row stops
#          failing, either the gap was fixed in the Go code (great — flip the
#          row to "pass" and update README.md) or the model drifted from the
#          code (bad — fix the model). Either way, a human must look, so the
#          script exits nonzero for that too.
#
# Requirements: spin (>= 6.x, the Bell Labs model checker) and a C compiler.
# Runtime: the full matrix is ~1 minute on a laptop.
set -u

cd "$(dirname "$0")" || exit 2

SPIN=${SPIN:-spin}
if ! command -v "$SPIN" >/dev/null 2>&1; then
    echo "error: spin not found (install: apt-get install spin / brew install spin)" >&2
    exit 2
fi
# Guard against Fermyon's unrelated "spin" CLI shadowing the model checker.
if ! "$SPIN" -V 2>/dev/null | grep -q "Spin Version"; then
    echo "error: '$SPIN -V' does not look like the Spin model checker" >&2
    exit 2
fi
CC=${CC:-}
if [ -z "$CC" ]; then
    CC=$(command -v gcc || command -v cc) || { echo "error: no C compiler" >&2; exit 2; }
fi
PAN_DEPTH=${PAN_DEPTH:-500000}

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

failures=0
printf '%-28s %-10s %-10s %12s\n' "RUN" "EXPECT" "RESULT" "STATES"
printf '%-28s %-10s %-10s %12s\n' "---" "------" "------" "------"

# run <name> <expect:pass|fail> <spin-defines> <cc-extra> <pan-args>
run() {
    local name=$1 expect=$2 defines=$3 ccextra=$4 panargs=$5
    local dir="$workdir/$name"
    mkdir -p "$dir"
    # shellcheck disable=SC2086  # defines/ccextra/panargs are word lists
    if ! (cd "$dir" && "$SPIN" -a $defines "$OLDPWD/escalation.pml" >/dev/null 2>&1 &&
          "$CC" -O2 -DCOLLAPSE $ccextra -o pan pan.c >/dev/null 2>&1); then
        printf '%-28s %-10s %-10s %12s\n' "$name" "$expect" "BUILD-ERR" "-"
        failures=$((failures + 1))
        return
    fi
    local out
    # shellcheck disable=SC2086  # panargs is a word list
    out=$(cd "$dir" && ./pan -m"$PAN_DEPTH" $panargs 2>&1)
    local states result verdict
    states=$(printf '%s\n' "$out" | sed -n 's/^ *\([0-9.e+]*\) states, stored.*/\1/p' | head -1)
    if printf '%s\n' "$out" | grep -q "errors: 0"; then
        result=pass
    elif printf '%s\n' "$out" | grep -Eq "errors: [1-9]"; then
        result=fail
    else
        result=inconclusive   # depth/memory limit hit before a verdict
    fi
    if [ "$result" = "$expect" ]; then
        verdict=$result
    else
        verdict="UNEXPECTED-$result"
        failures=$((failures + 1))
    fi
    printf '%-28s %-10s %-10s %12s\n' "$name" "$expect" "$verdict" "${states:-?}"
}

SAFETY="-DSAFETY -DNOCLAIM"
FAIR="-DNFAIR=12"

# --- properties expected to HOLD -------------------------------------------
run p1_p2_amnesty        pass "-DMON_AMNESTY"          "$SAFETY" ""
run p3_budget_asserts    pass "-DMON_BUDGET"           "$SAFETY" ""
run p3_cap_ltl           pass ""                       ""        "-a -N p3_cap"
run p3_total_ltl         pass "-DMON_BUDGET"           ""        "-a -N p3_total"
run p4a_p6_safety        pass ""                       "$SAFETY" ""
run p6_watcher_safety    pass "-DWATCHER"              "$SAFETY" ""
run w_onepass_acmm6      pass "-DMON_ADJ -DACMM6"      ""        "-a -N w_onepass"

# --- properties closed by gap fixes (previously pinned fails) ---------------
# #5511 closed G1/G3/G4; the #5617 follow-up closed G2.
# G1: Sweep's reviewer-verdict reconciliation (the former -DPATCH_REVIEWER
# hypothetical, now the shipped default) closes the P4b/P5 orphan path.
run p4b_handoff          pass ""                       "$FAIR"   "-a -f -N p4_handoff"
run p5_termination       pass ""                       "$FAIR"   "-a -f -N p5_term"
# G4: recommend-close marks the row; one adjudication ever, at every level.
run w_onepass_acmm5      pass "-DMON_ADJ"              ""        "-a -N w_onepass"
# G3: TryReEngage's escalated guard — no budget burned on needs-human PRs.
run w_watcher_reengage   pass "-DWATCHER"              ""        "-a -N w_watcher"
# G2 (#5617): pending observations are no-ops — the ledger survives CI windows.
run w_pending_wipe       pass "-DMON_PENDING"          ""        "-a -N w_pending"

echo
if [ "$failures" -ne 0 ]; then
    echo "FORMAL VERIFICATION: $failures run(s) diverged from the expected verdicts."
    exit 1
fi
echo "FORMAL VERIFICATION: all runs matched their expected verdicts."
