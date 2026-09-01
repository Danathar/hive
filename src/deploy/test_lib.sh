#!/usr/bin/env bash
# Shared skip discipline for the src/deploy/test_*.sh suites (#5388 item 3).
#
# ── The defect this exists to close ──────────────────────────────────────────
#
# A test block that skips on a runner that genuinely cannot run it is CORRECT.
# The same block skipping on a runner that CAN run it means the test is broken
# — and it reports green either way. That is the whole of #5388 item 3: a skip
# is a result nobody acts on, so a suite can quietly stop asserting anything
# and no lane ever goes red.
#
# #5383 solved this for exactly one case: HIVE_TEST_REQUIRE_BEHAVIOURAL=1 makes
# a skip fatal in the podman arm64 lane, where the container guarantees root and
# a `dev` account. It was never generalised — the helper was copy-pasted into
# two suites and nothing else. This file is that helper, extracted, so any suite
# can opt in and any lane whose runner GUARANTEES the precondition can demand it.
#
# ── The contract ─────────────────────────────────────────────────────────────
#
#   HIVE_TEST_REQUIRE_BEHAVIOURAL unset / 0 (default)
#       hive_test_skip prints "SKIP: <reason>" and the suite continues. Suites
#       stay runnable on a laptop and on a bare runner that lacks the tooling.
#
#   HIVE_TEST_REQUIRE_BEHAVIOURAL=1
#       hive_test_skip prints "FAIL: <reason>" and increments FAIL. The CALLER
#       is asserting the precondition holds, so a skip cannot mean "unsuitable
#       environment" — it means the precondition moved and the test silently
#       stopped testing.
#
# Setting the flag is a judgement call and belongs to the LANE, not the suite.
# Set it only where the runner guarantees the precondition. Setting it where the
# precondition is merely likely converts a correct skip into a false red.
#
# ── Usage ────────────────────────────────────────────────────────────────────
#
#   # shellcheck source=src/deploy/test_lib.sh
#   . "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"
#   ...
#   if ! command -v python3 >/dev/null 2>&1; then
#     hive_test_skip "python3 unavailable — cannot make structural assertions"
#     hive_test_report; exit $?          # honours the skip-as-failure result
#   fi
#
# Suites must define PASS and FAIL as integers before sourcing, or let this file
# initialise them (it does, if unset). hive_test_report prints the tally and
# RETURNS non-zero when FAIL > 0, so the suite's EXIT STATUS is the assertion —
# not a string a grep could match while nothing ran.

# Deliberately no `set -e` here: sourcing must not change the caller's shell
# options. Each suite keeps whatever discipline it already chose.

: "${PASS:=0}"
: "${FAIL:=0}"

# 1 when the caller asserts the preconditions for behavioural blocks are met.
HIVE_TEST_REQUIRE_BEHAVIOURAL="${HIVE_TEST_REQUIRE_BEHAVIOURAL:-0}"

# hive_test_require_behavioural — true when skips are fatal in this environment.
hive_test_require_behavioural() {
  [ "$HIVE_TEST_REQUIRE_BEHAVIOURAL" = "1" ]
}

# hive_test_pass <label>
hive_test_pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

# hive_test_fail <label> [detail]
hive_test_fail() {
  echo "  FAIL: $1"
  [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
  return 0
}

# hive_test_skip <reason> [detail]
#
# Permissive by default; FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1. Always
# returns 0 so `set -e` suites are not aborted mid-tally — the verdict is
# carried in FAIL and surfaced by hive_test_report's exit status.
hive_test_skip() {
  if hive_test_require_behavioural; then
    echo "  FAIL: $1"
    [ -n "${2:-}" ] && echo "        $2"
    echo "        HIVE_TEST_REQUIRE_BEHAVIOURAL=1 — the caller asserts this"
    echo "        precondition holds here, so this is a BROKEN TEST, not an"
    echo "        unsuitable environment (#5388)."
    FAIL=$((FAIL + 1))
  else
    echo "  SKIP: $1"
    [ -n "${2:-}" ] && echo "        $2"
  fi
  return 0
}

# hive_test_report — print the tally; return 0 only when nothing failed.
hive_test_report() {
  echo
  echo "=== $PASS passed, $FAIL failed ==="
  [ "$FAIL" -eq 0 ]
}
