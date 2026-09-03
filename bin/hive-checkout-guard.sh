#!/bin/bash
# hive-checkout-guard.sh — refuse to let a systemd unit execute code out of a
# checkout that an unprivileged local user could have planted (#5435).
#
# Usage: hive-checkout-guard.sh <directory> [entrypoint-file ...]
#
# WHY THIS EXISTS. Units like hive-discord.service run with
# WorkingDirectory=/tmp/hive/... and ExecStart=/usr/bin/node bot.js, so the code
# they execute is resolved out of a world-writable parent. /tmp's sticky bit
# only prevents deleting or renaming entries owned by SOMEONE ELSE; it does not
# prevent CREATING new ones. /tmp is also cleared on reboot, so after every boot
# there is a window in which /tmp/hive does not exist and any local user can
# create it — along with the file the unit is about to execute — before
# hive-deploy repopulates the real checkout. The service would then run attacker
# code as the service user, holding whatever EnvironmentFile grants it.
#
# Ordering directives do not help: Requires=/After= sequence startup, they say
# nothing about who owns the files.
#
# WHY A SCRIPT RATHER THAN INLINE ExecStartPre. The obvious one-liner
#
#     ExecStartPre=/usr/bin/find <dir> -maxdepth 0 -user dev ! -perm /go=w -print -quit
#
# is a NO-OP GUARD: find exits 0 after a successful traversal whether or not
# anything matched, so the "unsafe" case exits 0 too and systemd starts the unit
# anyway. This script's exit status is the assertion, and every failure path
# below exits non-zero, so a violation actually stops ExecStart from running.
#
# Test: bash src/deploy/test_hive_discord_unit_contract.sh
set -uo pipefail

DIR="${1:-}"
shift || true

die() { echo "hive-checkout-guard: REFUSING to start: $*" >&2; exit 1; }

# Portable stat helpers. GNU coreutils and BSD/macOS stat take different flags,
# and they disagree on the permission format: GNU '%a' includes the setuid/
# setgid/sticky bits when set (e.g. 1777), BSD '%Lp' prints only the low three
# (777), dropping sticky entirely. Inferring sticky from the digit count is
# therefore WRONG on BSD — it silently reports every sticky directory as
# non-sticky, which would reject /tmp itself. Sticky is read separately below
# via `test -k`, which both platforms implement.
stat_owner() { stat -c '%u' "$1" 2>/dev/null || stat -f '%u' "$1" 2>/dev/null; }
stat_perms() { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null; }

[ -n "$DIR" ] || die "no directory argument given"

# The service user this guard is asserting for. Resolved from the running EUID
# rather than hardcoded, so the check follows the unit's User= automatically.
ME="$(id -u)"

# Resolve without following a final symlink into somewhere else: a symlinked
# /tmp/hive/discord pointing at an attacker-owned tree would otherwise pass the
# ownership test on its target while the unit executes the target's code.
[ -L "$DIR" ] && die "$DIR is a symlink — refusing to execute out of it"
[ -d "$DIR" ] || die "$DIR does not exist or is not a directory"

# Every component from / down to DIR must be safe. Checking only the leaf is not
# enough: if /tmp/hive is attacker-owned they can swap the whole discord/
# subtree, and a leaf-only check would happily validate the replacement.
node="$(cd "$DIR" 2>/dev/null && pwd -P)" || die "cannot resolve $DIR"
while : ; do
  owner="$(stat_owner "$node")" || die "cannot stat $node"
  perms="$(stat_perms "$node")" || die "cannot stat $node"
  [ -n "$owner" ] && [ -n "$perms" ] || die "cannot stat $node"

  # Owned by us or by root. root is accepted because the deploy tooling runs
  # some steps under sudo; anything else means a third party controls the path.
  if [ "$owner" != "$ME" ] && [ "$owner" != "0" ]; then
    die "$node is owned by uid $owner (expected $ME or root)"
  fi

  # Group- or other-writable is only tolerable with the sticky bit, which is
  # what makes /tmp itself acceptable as an ANCESTOR: sticky means other users
  # cannot replace entries they do not own. A non-sticky world-writable
  # directory anywhere on the path means the tree can be swapped underneath us.
  #
  # perms is 3 or 4 octal digits depending on platform; take the low three as
  # the mode and read sticky separately (see stat_perms above).
  mode="${perms: -3}"
  sticky=0
  [ -k "$node" ] && sticky=1
  go_w=0
  case "${mode:1:1}" in 2|3|6|7) go_w=1 ;; esac
  case "${mode:2:1}" in 2|3|6|7) go_w=1 ;; esac

  if [ "$go_w" = 1 ] && [ "$sticky" != 1 ]; then
    die "$node is group/other-writable ($perms) without the sticky bit"
  fi

  # The LEAF must not be world-writable at all, sticky or not. Sticky protects
  # existing entries from replacement, but the leaf is where new files appear,
  # and a new bot.js is exactly the attack.
  if [ "$node" = "$(cd "$DIR" && pwd -P)" ] && [ "$go_w" = 1 ]; then
    die "$DIR is group/other-writable ($perms) — anyone could add files to it"
  fi

  [ "$node" = "/" ] && break
  parent="$(dirname "$node")"
  [ "$parent" = "$node" ] && break
  node="$parent"
done

# Each named entrypoint must exist, be a regular file, be owned by us or root,
# and not be writable by anyone else.
for f in "$@"; do
  path="$DIR/$f"
  [ -L "$path" ] && die "$path is a symlink — refusing to execute it"
  [ -f "$path" ] || die "$path does not exist or is not a regular file"

  owner="$(stat_owner "$path")" || die "cannot stat $path"
  perms="$(stat_perms "$path")" || die "cannot stat $path"
  [ -n "$owner" ] && [ -n "$perms" ] || die "cannot stat $path"

  if [ "$owner" != "$ME" ] && [ "$owner" != "0" ]; then
    die "$path is owned by uid $owner (expected $ME or root)"
  fi

  mode="${perms: -3}"
  case "${mode:1:1}" in 2|3|6|7) die "$path is group-writable ($perms)" ;; esac
  case "${mode:2:1}" in 2|3|6|7) die "$path is world-writable ($perms)" ;; esac
done

exit 0
