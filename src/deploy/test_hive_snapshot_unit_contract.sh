#!/usr/bin/env bash
# hive-snapshot.service must not execute code out of a directory a local user
# could have planted (#5483, same class as #5435).
# Run: bash src/deploy/test_hive_snapshot_unit_contract.sh
#
# WHAT THE BUG WAS. The unit ran:
#
#     User=dev
#     ExecStart=/tmp/hive/dashboard/publish-snapshot.sh
#
# so the executed script resolved out of a world-writable parent that is
# CLEARED ON REBOOT. /tmp's sticky bit only stops a user from deleting or
# renaming entries owned by someone else; it does not stop them creating
# /tmp/hive/dashboard/publish-snapshot.sh in the window between the boot that
# wipes /tmp and hive-deploy repopulating the checkout. When
# hive-snapshot.timer next fired, the winner of that race got code execution
# as `dev` — and the real script reads the GitHub App token from
# /var/run/hive-metrics/gh-app-token.cache, so the planted code runs with a
# path to that credential.
#
# WHY THIS TEST RUNS THE GUARD INSTEAD OF ONLY GREPPING THE UNIT. A grep for
# "ExecStartPre" would pass against a guard that never rejects anything —
# exactly the no-op-guard failure #5398 found in another contract test. So
# besides pinning the unit's directives, this EXECUTES bin/hive-checkout-guard.sh
# with the unit's actual arguments against real directory trees and asserts on
# its EXIT STATUS. The exhaustive per-attack-shape matrix already lives in
# src/deploy/test_hive_discord_unit_contract.sh; this test re-runs the shapes
# specific to THIS unit: the entrypoint is the executed script itself (shipped
# mode 755), so the writable-file check — not the exec-bit — is the live one.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT="${ROOT}/systemd/hive-snapshot.service"
GUARD="${ROOT}/bin/hive-checkout-guard.sh"

echo "=== hive-snapshot.service does not execute planted code (#5483) ==="

for f in "$UNIT" "$GUARD"; do
  if [ ! -f "$f" ]; then
    fail "locate $f" "the layout moved — this test cannot verify anything"
    echo ""
    echo "=== Results: $PASS passed, $FAIL failed ==="
    exit 1
  fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- the unit wires the guard in ---------------------------------------------
echo ""
echo "--- the unit file ---"

GUARD_LINE="$(grep -E '^ExecStartPre=' "$UNIT" || true)"
if [ -z "$GUARD_LINE" ]; then
  fail "hive-snapshot.service has an ExecStartPre guard" \
       "without one, systemd executes whatever publish-snapshot.sh is present when the timer fires"
else
  pass "hive-snapshot.service has an ExecStartPre guard"
  if printf '%s' "$GUARD_LINE" | grep -q 'hive-checkout-guard.sh'; then
    pass "the guard is hive-checkout-guard.sh (exit status is the assertion)"
  else
    fail "the guard is hive-checkout-guard.sh" "got: ${GUARD_LINE}"
  fi
  # The guard must be told the exact file ExecStart runs. ExecStart here is an
  # absolute path; the guard takes <dir> <basename>, so both derived parts must
  # appear in the guard invocation or it validates something else entirely.
  EXEC_PATH="$(grep -E '^ExecStart=' "$UNIT" | head -1 | cut -d= -f2-)"
  EXEC_DIR="$(dirname "$EXEC_PATH")"
  EXEC_FILE="$(basename "$EXEC_PATH")"
  if printf '%s' "$GUARD_LINE" | grep -qF -- "$EXEC_DIR" && \
     printf '%s' "$GUARD_LINE" | grep -qF -- "$EXEC_FILE"; then
    pass "the guard checks the directory and file ExecStart actually runs (${EXEC_PATH})"
  else
    fail "the guard checks the directory and file ExecStart runs (${EXEC_PATH})" "got: ${GUARD_LINE}"
  fi
fi

# PrivateTmp would give the unit a private /tmp, hiding the very checkout
# ExecStart points at — the unit could never start. Asserting its ABSENCE keeps
# a future "more hardening is better" edit from silently breaking snapshots.
if grep -qE '^PrivateTmp=(yes|true|1)' "$UNIT"; then
  fail "PrivateTmp is not set on this unit" \
       "ExecStart is under /tmp; a private /tmp hides the checkout and the unit cannot start"
else
  pass "PrivateTmp is not set (it would hide the /tmp checkout this unit runs from)"
fi

for d in NoNewPrivileges ProtectSystem; do
  if grep -qE "^${d}=" "$UNIT"; then
    pass "${d} is set"
  else
    fail "${d} is set" "unit lost a hardening directive"
  fi
done

# ProtectSystem=strict would make /tmp read-only for the service, and the
# script builds the snapshot under /tmp. Same trap as PrivateTmp.
if grep -qE '^ProtectSystem=strict' "$UNIT"; then
  fail "ProtectSystem is not 'strict'" "strict mounts /tmp read-only; this unit builds under /tmp"
else
  pass "ProtectSystem is not 'strict' (which would make the /tmp checkout read-only)"
fi

# --- the guard, run with this unit's argument shape ---------------------------
echo ""
echo "--- the guard, run against real trees ---"

# Build a tree shaped like the real one: a sticky world-writable ancestor
# standing in for /tmp, with the checkout underneath it and the entrypoint
# shipped exactly as git ships it — an executable 755 script.
#   $WORK/tmp/hive/dashboard/publish-snapshot.sh
mk_tree() {
  rm -rf "${WORK}/tmp"
  mkdir -p "${WORK}/tmp/hive/dashboard"
  chmod 1777 "${WORK}/tmp"
  chmod 755 "${WORK}/tmp/hive" "${WORK}/tmp/hive/dashboard"
  : > "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
  chmod 755 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
}

expect() {
  local want="$1" label="$2"
  local out rc
  out="$(bash "$GUARD" "${WORK}/tmp/hive/dashboard" publish-snapshot.sh 2>&1)"
  rc=$?
  if [ "$rc" -eq "$want" ]; then
    if [ "$want" -eq 0 ]; then
      pass "${label}: guard allows startup (rc=0)"
    else
      pass "${label}: guard REFUSES startup (rc=${rc})"
    fi
  else
    fail "${label}: expected rc=${want}, got rc=${rc}" "${out}"
  fi
}

# The good state: a healthy checkout with the script at its shipped 755 mode.
# If this fails the guard is too strict and snapshots stop on a healthy host.
mk_tree
expect 0 "healthy checkout under a sticky /tmp (script mode 755)"

# THE VULNERABILITY, reproduced: /tmp wiped by a reboot, checkout not yet
# restored. Before this fix, the next timer fire ran whatever was there.
mk_tree
rm -rf "${WORK}/tmp/hive"
mkdir -p "${WORK}/tmp/hive/dashboard"
: > "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
chmod 777 "${WORK}/tmp/hive" "${WORK}/tmp/hive/dashboard"
chmod 755 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "post-reboot window, attacker-shaped world-writable checkout (the #5483 race)"

# The entrypoint is a 755 SCRIPT, so unlike bot.js the writable-file check is
# the one that matters here: a group/world-writable script can be rewritten in
# place between guard check and the next timer fire.
mk_tree
chmod 775 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is group-writable"

mk_tree
chmod 777 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is world-writable"

# A symlinked entrypoint redirects an otherwise-clean path at attacker code.
mk_tree
rm -f "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
: > "${WORK}/evil.sh"
chmod 755 "${WORK}/evil.sh"
ln -s "${WORK}/evil.sh" "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is a symlink"

# Missing checkout is a refusal, not a skip.
mk_tree
rm -rf "${WORK}/tmp/hive/dashboard"
expect 1 "the checkout directory does not exist"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
