#!/usr/bin/env bash
# hive-discord.service must not execute code out of a directory a local user
# could have planted (#5435).
# Run: bash src/deploy/test_hive_discord_unit_contract.sh
#
# WHAT THE BUG WAS. The unit ran:
#
#     WorkingDirectory=/tmp/hive/discord
#     ExecStart=/usr/bin/node bot.js
#     EnvironmentFile=/etc/hive/discord.env
#
# so bot.js resolved out of a world-writable parent that is CLEARED ON REBOOT.
# /tmp's sticky bit only stops a user from deleting or renaming entries owned by
# someone else; it does not stop them creating /tmp/hive/discord/bot.js in the
# window between the boot that wipes /tmp and hive-deploy repopulating the
# checkout. The winner of that race gets code execution as `dev` holding the
# Discord bot token. Requires=hive.service orders startup and validates nothing.
#
# WHY THIS TEST RUNS THE GUARD INSTEAD OF GREPPING THE UNIT. A grep for
# "ExecStartPre" would pass against a guard that never rejects anything — and
# that is not hypothetical here. The obvious inline form,
#
#     ExecStartPre=/usr/bin/find <dir> -maxdepth 0 -user dev ! -perm /go=w -print -quit
#
# exits 0 whether or not it matched, so the unsafe case starts the unit anyway:
# a no-op guard that greps green, which is exactly the failure #5398 found in
# another contract test. So the assertions below EXECUTE bin/hive-checkout-guard.sh
# against real directory trees and assert on its EXIT STATUS, once per attack
# shape, including the pre-fix arrangement which must be rejected.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT="${ROOT}/systemd/hive-discord.service"
GUARD="${ROOT}/bin/hive-checkout-guard.sh"

echo "=== hive-discord.service does not execute planted code (#5435) ==="

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

# Read the directive out of the unit rather than restating it, so a unit that
# drops the guard fails here instead of silently passing the behaviour tests.
GUARD_LINE="$(grep -E '^ExecStartPre=' "$UNIT" || true)"
if [ -z "$GUARD_LINE" ]; then
  fail "hive-discord.service has an ExecStartPre guard" \
       "without one, systemd executes whatever bot.js is present at start time"
else
  pass "hive-discord.service has an ExecStartPre guard"
  if printf '%s' "$GUARD_LINE" | grep -q 'hive-checkout-guard.sh'; then
    pass "the guard is hive-checkout-guard.sh (exit status is the assertion)"
  else
    fail "the guard is hive-checkout-guard.sh" "got: ${GUARD_LINE}"
  fi
  # The guard must be told which file ExecStart will run. Checking the directory
  # alone would let a missing/planted bot.js through.
  EXEC_FILE="$(grep -E '^ExecStart=' "$UNIT" | sed 's/.* //')"
  if printf '%s' "$GUARD_LINE" | grep -qF -- "$EXEC_FILE"; then
    pass "the guard checks the file ExecStart actually runs (${EXEC_FILE})"
  else
    fail "the guard checks the file ExecStart runs (${EXEC_FILE})" "got: ${GUARD_LINE}"
  fi
fi

# PrivateTmp would give the unit a private /tmp, hiding the very checkout
# WorkingDirectory points at — the service would fail to start. Asserting its
# ABSENCE keeps a future "more hardening is better" edit from breaking the bot.
if grep -qE '^PrivateTmp=(yes|true|1)' "$UNIT"; then
  fail "PrivateTmp is not set on this unit" \
       "WorkingDirectory is under /tmp; a private /tmp hides the checkout and the unit cannot start"
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

# ProtectSystem=strict would make /tmp read-only for the service. Same trap.
if grep -qE '^ProtectSystem=strict' "$UNIT"; then
  fail "ProtectSystem is not 'strict'" "strict mounts /tmp read-only; this unit runs out of /tmp"
else
  pass "ProtectSystem is not 'strict' (which would make the /tmp checkout read-only)"
fi

# --- the guard actually rejects things ---------------------------------------
echo ""
echo "--- the guard, run against real trees ---"

# Build a tree shaped like the real one: a sticky world-writable ancestor
# standing in for /tmp, with the checkout underneath it.
#   $WORK/tmp/hive/discord/bot.js
mk_tree() {
  rm -rf "${WORK}/tmp"
  mkdir -p "${WORK}/tmp/hive/discord"
  chmod 1777 "${WORK}/tmp"
  chmod 755 "${WORK}/tmp/hive" "${WORK}/tmp/hive/discord"
  : > "${WORK}/tmp/hive/discord/bot.js"
  chmod 644 "${WORK}/tmp/hive/discord/bot.js"
}

# expect <rc> <label> -- runs the guard exactly as the unit does.
expect() {
  local want="$1" label="$2"
  local out rc
  out="$(bash "$GUARD" "${WORK}/tmp/hive/discord" bot.js 2>&1)"
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

# The good state. If this ever fails the guard is too strict and would take the
# bot down on a healthy host — a worse outcome than the bug being fixed.
mk_tree
expect 0 "healthy checkout under a sticky /tmp"

# THE VULNERABILITY, reproduced. This is the pre-fix arrangement: /tmp wiped by
# a reboot, the checkout not yet restored, so the directory the unit executes
# from is one any local user can create and fill. Before this fix systemd ran
# whatever it found; the guard must refuse.
mk_tree
rm -f "${WORK}/tmp/hive/discord/bot.js"
expect 1 "post-reboot window, bot.js absent (the #5435 race)"

# The attacker's own tree: they got there first and it is theirs to rewrite.
mk_tree
chmod 777 "${WORK}/tmp/hive/discord"
expect 1 "checkout directory is world-writable"

# Sticky on the LEAF must not buy an exemption. Sticky protects existing entries
# from replacement, but new files are exactly the attack, and bot.js is new.
mk_tree
chmod 1777 "${WORK}/tmp/hive/discord"
expect 1 "checkout directory is world-writable even with the sticky bit"

# An intermediate the attacker controls lets them swap the whole subtree, so a
# leaf-only check is not enough.
mk_tree
chmod 777 "${WORK}/tmp/hive"
expect 1 "an ancestor is world-writable without the sticky bit"

mk_tree
chmod 775 "${WORK}/tmp/hive"
expect 1 "an ancestor is group-writable"

# A writable bot.js can be rewritten in place between the check and the next
# restart, so it is rejected even inside an otherwise-safe directory.
mk_tree
chmod 666 "${WORK}/tmp/hive/discord/bot.js"
expect 1 "bot.js is world-writable"

mk_tree
chmod 664 "${WORK}/tmp/hive/discord/bot.js"
expect 1 "bot.js is group-writable"

# Symlinks are how an attacker redirects an otherwise-clean path at their own
# code, so neither the directory nor the entrypoint may be one.
mk_tree
rm -f "${WORK}/tmp/hive/discord/bot.js"
: > "${WORK}/evil.js"
ln -s "${WORK}/evil.js" "${WORK}/tmp/hive/discord/bot.js"
expect 1 "bot.js is a symlink"

mk_tree
mv "${WORK}/tmp/hive/discord" "${WORK}/elsewhere"
ln -s "${WORK}/elsewhere" "${WORK}/tmp/hive/discord"
expect 1 "the checkout directory is a symlink"

# The guard must not be fooled into passing when it cannot tell — a missing
# directory is a refusal, not a skip.
mk_tree
rm -rf "${WORK}/tmp/hive/discord"
expect 1 "the checkout directory does not exist"

# --- the guard is installed before anything references it --------------------
echo ""
echo "--- deploy bootstraps the guard ---"

# hive-deploy.sh's sync loops both skip files that are not already installed, so
# a NEW helper is never bootstrapped by them. The unit's ExecStartPre points at
# /usr/local/bin/hive-checkout-guard.sh; if deploy cannot place it there, an
# upgraded host gets a unit calling a script it does not have and the bot stops.
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
       "the drift loops skip files that are not already installed, so an upgraded host would never receive it and hive-discord.service would fail to start"
fi

if [ -x "$GUARD" ]; then
  pass "bin/hive-checkout-guard.sh is executable in git"
else
  fail "bin/hive-checkout-guard.sh is executable in git" "mode is $(ls -l "$GUARD" | cut -d' ' -f1)"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
