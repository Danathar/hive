#!/usr/bin/env bash
# hive-snapshot.service must not execute code out of a directory a local user
# could have planted (#5483, same class as #5435).
# Run: bash src/deploy/test_hive_snapshot_unit_contract.sh
#
# WHAT THE BUG WAS. The unit ran:
#
#     Type=oneshot
#     User=dev
#     ExecStart=/tmp/hive/dashboard/publish-snapshot.sh
#
# so the executed script resolved out of a world-writable parent that is CLEARED
# ON REBOOT. /tmp's sticky bit only stops a user from deleting or renaming
# entries owned by someone else; it does not stop them creating
# /tmp/hive/dashboard/publish-snapshot.sh in the window between the boot that
# wipes /tmp and hive-deploy repopulating the checkout. The winner of that race
# gets code execution as `dev` the next time hive-snapshot.timer fires.
#
# Severity is lower than #5435 — oneshot on a 15-minute timer rather than
# Restart=always, and no EnvironmentFile hands it a token — but not zero: the
# script reads a GitHub App token from /var/run/hive-metrics/gh-app-token.cache
# and pushes to kubestellar/docs.
#
# WHY THIS TEST RUNS THE GUARD INSTEAD OF GREPPING THE UNIT. A grep for
# "ExecStartPre" would pass against a guard that never rejects anything — and
# that is not hypothetical: the obvious inline form,
#
#     ExecStartPre=/usr/bin/find <dir> -maxdepth 0 -user dev ! -perm /go=w -print -quit
#
# exits 0 whether or not it matched, so the unsafe case starts the unit anyway.
# So the assertions below EXECUTE bin/hive-checkout-guard.sh against real
# directory trees and assert on its EXIT STATUS, once per attack shape,
# including the pre-fix arrangement which must be rejected.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT="${ROOT}/systemd/hive-snapshot.service"
GUARD="${ROOT}/bin/hive-checkout-guard.sh"
SCRIPT="${ROOT}/dashboard/publish-snapshot.sh"

echo "=== hive-snapshot.service does not execute planted code (#5483) ==="

for f in "$UNIT" "$GUARD" "$SCRIPT"; do
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

# Read the directive out of the unit rather than restating it, so a unit that
# drops the guard fails here instead of silently passing the behaviour tests.
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

  # The guard must be told which file ExecStart will run. Checking the directory
  # alone would let a planted publish-snapshot.sh through. Unlike the discord
  # unit, ExecStart here is an ABSOLUTE path (there is no WorkingDirectory), so
  # the directory and basename are derived from it and both must appear in the
  # guard line — otherwise a future edit could point ExecStart at one path while
  # the guard keeps validating another.
  EXEC_PATH="$(grep -E '^ExecStart=' "$UNIT" | head -1 | sed 's/^ExecStart=//')"
  EXEC_DIR="$(dirname "$EXEC_PATH")"
  EXEC_FILE="$(basename "$EXEC_PATH")"
  if printf '%s' "$GUARD_LINE" | grep -qF -- "$EXEC_FILE"; then
    pass "the guard checks the file ExecStart actually runs (${EXEC_FILE})"
  else
    fail "the guard checks the file ExecStart runs (${EXEC_FILE})" "got: ${GUARD_LINE}"
  fi
  if printf '%s' "$GUARD_LINE" | grep -qF -- "$EXEC_DIR"; then
    pass "the guard checks the directory ExecStart runs from (${EXEC_DIR})"
  else
    fail "the guard checks the directory ExecStart runs from (${EXEC_DIR})" "got: ${GUARD_LINE}"
  fi
fi

# PrivateTmp would give the unit a private /tmp, hiding BOTH the checkout
# ExecStart runs from and DOCS_REPO_DIR=/tmp/kubestellar-docs-snapshot, which
# the script clones into. Asserting its ABSENCE keeps a future "more hardening
# is better" edit from breaking the snapshot job.
if grep -qE '^PrivateTmp=(yes|true|1)' "$UNIT"; then
  fail "PrivateTmp is not set on this unit" \
       "ExecStart and DOCS_REPO_DIR are both under /tmp; a private /tmp hides them and the unit cannot work"
else
  pass "PrivateTmp is not set (it would hide the /tmp checkout and the /tmp docs clone)"
fi

for d in NoNewPrivileges ProtectSystem; do
  if grep -qE "^${d}=" "$UNIT"; then
    pass "${d} is set"
  else
    fail "${d} is set" "unit lost a hardening directive"
  fi
done

# ProtectSystem=strict would make /tmp read-only for the service. Same trap.
if grep -qE '^ProtectSystem=strict' "$UNIT"; then
  fail "ProtectSystem is not 'strict'" "strict mounts /tmp read-only; this unit runs out of /tmp and writes a git clone there"
else
  pass "ProtectSystem is not 'strict' (which would make the /tmp checkout read-only)"
fi

# ProtectHome=read-only is correct for hive-discord.service but WRONG here, and
# this assertion records why so the "make the units consistent" follow-up does
# not quietly break the snapshot push. publish-snapshot.sh drives git and gh,
# both of which write under $HOME.
if grep -qE '^ProtectHome=(read-only|yes|true)' "$UNIT"; then
  fail "ProtectHome does not make \$HOME read-only for this unit" \
       "publish-snapshot.sh runs git and gh, which write to \$HOME (gh config/state, git lock files)"
else
  pass "ProtectHome is not read-only (git and gh in publish-snapshot.sh write to \$HOME)"
fi

# --- the guard actually rejects things ---------------------------------------
echo ""
echo "--- the guard, run against real trees ---"

# Build a tree shaped like the real one: a sticky world-writable ancestor
# standing in for /tmp, with the checkout underneath it.
#   $WORK/tmp/hive/dashboard/publish-snapshot.sh
#
# Note the mode: publish-snapshot.sh ships 755 in git, unlike bot.js which is
# 644. The guard's group/other-WRITABLE check is what matters here — the exec
# bit is irrelevant to it — so 755 must be accepted.
mk_tree() {
  rm -rf "${WORK}/tmp"
  mkdir -p "${WORK}/tmp/hive/dashboard"
  chmod 1777 "${WORK}/tmp"
  chmod 755 "${WORK}/tmp/hive" "${WORK}/tmp/hive/dashboard"
  : > "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
  chmod 755 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
}

# expect <rc> <label> -- runs the guard exactly as the unit does.
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

# The good state. If this ever fails the guard is too strict and would stop the
# snapshot timer on a healthy host — a worse outcome than the bug being fixed.
# This also pins that a 755 entrypoint passes: the file ships executable, so a
# guard that rejected the exec bit would break every run.
mk_tree
expect 0 "healthy checkout under a sticky /tmp, entrypoint 755 as shipped"

# THE VULNERABILITY, reproduced. This is the pre-fix arrangement: /tmp wiped by
# a reboot, the checkout not yet restored, so the directory the unit executes
# from is one any local user can create and fill. Before this fix systemd ran
# whatever it found when the timer fired; the guard must refuse.
mk_tree
rm -f "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "post-reboot window, publish-snapshot.sh absent (the #5483 race)"

# The attacker's own tree: they got there first and it is theirs to rewrite.
mk_tree
chmod 777 "${WORK}/tmp/hive/dashboard"
expect 1 "checkout directory is world-writable"

# Sticky on the LEAF must not buy an exemption. Sticky protects existing entries
# from replacement, but new files are exactly the attack.
mk_tree
chmod 1777 "${WORK}/tmp/hive/dashboard"
expect 1 "checkout directory is world-writable even with the sticky bit"

# An intermediate the attacker controls lets them swap the whole subtree, so a
# leaf-only check is not enough.
mk_tree
chmod 777 "${WORK}/tmp/hive"
expect 1 "an ancestor is world-writable without the sticky bit"

mk_tree
chmod 775 "${WORK}/tmp/hive"
expect 1 "an ancestor is group-writable"

# A writable entrypoint can be rewritten in place before the next timer firing,
# so it is rejected even inside an otherwise-safe directory. 757/775 rather than
# 666/664 because this file legitimately carries the exec bit — the guard must
# key on the WRITE bits, not on mode equality.
mk_tree
chmod 757 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is world-writable (and executable, as it ships)"

mk_tree
chmod 775 "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is group-writable (and executable, as it ships)"

# Symlinks are how an attacker redirects an otherwise-clean path at their own
# code, so neither the directory nor the entrypoint may be one.
mk_tree
rm -f "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
printf '#!/bin/sh\n' > "${WORK}/evil.sh"
chmod 755 "${WORK}/evil.sh"
ln -s "${WORK}/evil.sh" "${WORK}/tmp/hive/dashboard/publish-snapshot.sh"
expect 1 "publish-snapshot.sh is a symlink"

mk_tree
mv "${WORK}/tmp/hive/dashboard" "${WORK}/elsewhere"
ln -s "${WORK}/elsewhere" "${WORK}/tmp/hive/dashboard"
expect 1 "the checkout directory is a symlink"

# The guard must not be fooled into passing when it cannot tell — a missing
# directory is a refusal, not a skip.
mk_tree
rm -rf "${WORK}/tmp/hive/dashboard"
expect 1 "the checkout directory does not exist"

# --- the guard is installed before anything references it --------------------
echo ""
echo "--- deploy bootstraps the guard ---"

# hive-deploy.sh's sync loops both skip files that are not already installed, so
# a NEW helper is never bootstrapped by them. #5481 added the explicit install
# for hive-discord.service; this unit's ExecStartPre depends on exactly the same
# file, so the assertion is repeated here rather than assumed — if that block is
# ever removed, BOTH units stop starting and both tests should say so.
DEPLOY="${ROOT}/bin/hive-deploy.sh"
if [ ! -f "$DEPLOY" ]; then
  fail "locate bin/hive-deploy.sh" "cannot verify the guard is installed"
elif grep -q 'hive-checkout-guard.sh' "$DEPLOY"; then
  pass "hive-deploy.sh installs hive-checkout-guard.sh explicitly"
  if grep -A5 'CHECKOUT_GUARD_SRC=' "$DEPLOY" | grep -q 'install -m 0755'; then
    pass "and installs it executable (0755)"
  else
    fail "the guard is installed executable" "ExecStartPre needs the exec bit"
  fi
else
  fail "hive-deploy.sh installs hive-checkout-guard.sh" \
       "the drift loops skip files that are not already installed, so an upgraded host would never receive it and hive-snapshot.service would fail to start"
fi

if [ -x "$GUARD" ]; then
  pass "bin/hive-checkout-guard.sh is executable in git"
else
  fail "bin/hive-checkout-guard.sh is executable in git" "mode is $(ls -l "$GUARD" | cut -d' ' -f1)"
fi

# The real entrypoint must itself be executable in git, since ExecStart invokes
# it directly rather than through an interpreter.
if [ -x "$SCRIPT" ]; then
  pass "dashboard/publish-snapshot.sh is executable in git"
else
  fail "dashboard/publish-snapshot.sh is executable in git" \
       "ExecStart runs it directly, so a non-executable file fails at start"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
