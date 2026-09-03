#!/usr/bin/env bash
# #5369: /data ownership must be an INVARIANT, not a boot-time snapshot.
#
# The fault this closes: entrypoint.sh's `chown -R dev:node /data` is guarded on
# `[ "$DATA_OWNER" != "1001" ]`, and src/Dockerfile already ships /data owned by
# dev:node — so on a normal boot the guard is FALSE and the recursive chown never
# runs. Everything the root phase creates under /data afterwards keeps root:root,
# and the hive process (uid 1001, after the setpriv/gosu drop) cannot read it.
#
# #5360 was one instance: /data/hive.yaml.runtime, chmod 600 with no chown, in
# the root phase, on a /data that was already uid 1001. #5368 fixed that one
# file. This tests the CLASS.
#
# The standard here is #5368's, and it is deliberate: assert READABILITY BY THE
# READING USER, not permission bits. #5342 asserted only the mode and passed
# while the product was broken — mode 0600 is perfectly correct and perfectly
# unreadable when the owner is not the reader.
#
# Run: bash src/deploy/test_entrypoint_data_ownership.sh
set -uo pipefail

# Shared skip discipline (#5388): hive_test_skip is permissive by default and
# FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1. Extracted from this file and its
# sibling by #5388 so every deploy suite can use the same contract.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"
RUNTIME_UID=1001

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want: '$want'"
    echo "        got:  '$got'"
    FAIL=$((FAIL + 1))
  fi
}

ok()   { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #5369: /data ownership invariant ==="

# ── Structural: the guard must SURVIVE ───────────────────────────────────
#
# The fix must not be "delete the guard". A recursive chown over an NFS-backed
# PVC with thousands of files costs minutes of startup; removing the guard
# trades a permissions bug for a boot-time one. Assert the protection is still
# there, so a future "simplification" that removes it fails here.
if grep -q 'DATA_OWNER=\$(stat -c' "$ENTRYPOINT" \
   && grep -q 'if \[ "\$DATA_OWNER" != "1001" \]; then' "$ENTRYPOINT"; then
  ok "the DATA_OWNER guard still gates the recursive chown (NFS protection intact)"
else
  bad "the DATA_OWNER guard is gone" \
      "removing it reintroduces multi-minute NFS startup delays; the fix for #5369 is a targeted sweep, not a walk"
fi

# The sweep that replaces it must NOT itself be a recursive walk, or it has
# reintroduced exactly the cost the guard exists to avoid.
SWEEP="$(sed -n '/^hive_sweep_root_phase_paths() {/,/^}/p' "$ENTRYPOINT")"
if [ -z "$SWEEP" ]; then
  bad "could not extract hive_sweep_root_phase_paths from $ENTRYPOINT"
elif grep -qE 'chown[[:space:]]+-R|chown[[:space:]]+-[a-zA-Z]*R' <<<"$SWEEP"; then
  bad "the ownership sweep recurses" \
      "a recursive chown here costs the same multi-minute NFS walk the DATA_OWNER guard prevents"
else
  ok "the ownership sweep is non-recursive (cost does not scale with PVC size)"
fi

# ── Structural: the path list must be wired to BOTH consumers ────────────
for fn in hive_sweep_root_phase_paths hive_assert_runtime_readable; do
  if grep -q "^${fn}() {" "$ENTRYPOINT"; then
    ok "$fn is defined"
  else
    bad "$fn is not defined in $ENTRYPOINT"
  fi
done

if grep -q '^HIVE_DATA_ROOT_PHASE_PATHS="' "$ENTRYPOINT"; then
  ok "HIVE_DATA_ROOT_PHASE_PATHS is defined"
else
  bad "HIVE_DATA_ROOT_PHASE_PATHS is not defined"
fi

# The sweep must actually be CALLED in the root phase. A helper that is defined
# and never invoked is the #5369 shape all over again — code that looks like a
# fix and runs on no boot at all.
sweep_calls="$(grep -cE '^[[:space:]]*hive_sweep_root_phase_paths[[:space:]]*$' "$ENTRYPOINT" || true)"
check "the sweep is invoked exactly once" "1" "$sweep_calls"

# ── Structural: the assertion must run BEFORE the privilege drop ─────────
#
# After the exec we are uid 1001 and can no longer chown or meaningfully
# diagnose. An assertion placed after the drop would never run at all.
assert_line="$(grep -n 'hive_assert_runtime_readable \$HIVE_DATA_ROOT_PHASE_PATHS' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
drop_line="$(grep -nE '^[[:space:]]*exec (setpriv|gosu) ' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
if [ -z "$assert_line" ]; then
  bad "hive_assert_runtime_readable is never called on the path list"
elif [ -z "$drop_line" ]; then
  bad "could not locate the privilege drop (exec setpriv/gosu)"
elif [ "$assert_line" -lt "$drop_line" ]; then
  ok "the readability assertion runs before the privilege drop (line $assert_line < $drop_line)"
else
  bad "the readability assertion runs AFTER the privilege drop" \
      "at that point the process is already uid 1001 — the check cannot fire or fix anything"
fi

# The assertion must cover the config paths, which are the ones whose failure
# is fatal. #5360 was exactly /data/hive.yaml.runtime.
if grep -q 'hive_assert_runtime_readable \$HIVE_DATA_ROOT_PHASE_PATHS' "$ENTRYPOINT" \
   && sed -n "${assert_line},$((assert_line + 2))p" "$ENTRYPOINT" | grep -q 'HIVE_CONFIG_RUNTIME'; then
  ok "the assertion covers the runtime config paths (the #5360 file)"
else
  bad "the assertion does not cover HIVE_CONFIG_RUNTIME" \
      "that is the file whose unreadability was #5360; it is the one that must be named"
fi

# ── Structural: every root-phase /data creator is in the list ────────────
#
# This is the maintenance rule enforced mechanically. Extract the root phase
# (from `if [ "$(id -u)" = "0" ]` to the privilege drop) and find every mkdir
# that creates a path under /data. Each such path must either appear in
# HIVE_DATA_ROOT_PHASE_PATHS or be explicitly chowned at its own site.
#
# Without this, the list silently goes stale the first time someone adds a
# write — which is precisely the failure mode the issue describes.
echo
echo "=== every root-phase /data creator is covered ==="

root_start="$(grep -n 'if \[ "\$(id -u)" = "0" \]; then' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
if [ -z "$root_start" ] || [ -z "$drop_line" ]; then
  bad "could not delimit the root phase"
else
  ROOT_PHASE="$(sed -n "${root_start},${drop_line}p" "$ENTRYPOINT")"
  LIST="$(sed -n '/^HIVE_DATA_ROOT_PHASE_PATHS="/,/^"$/p' "$ENTRYPOINT")"

  # Paths intentionally excluded, with the reason recorded in the entrypoint:
  #   /data/agents/*, /data/beads/*  -> chowned to per-agent hive-<name> UIDs
  #   /data/vaults                   -> created in the DEV phase, already dev-owned
  uncovered=""
  # shellcheck disable=SC2016
  mkdir_paths="$(printf '%s' "$ROOT_PHASE" \
    | grep -oE 'mkdir -p [^&|;]*' \
    | tr ' ' '\n' \
    | grep -E '^/data/[A-Za-z0-9._/-]+$' \
    | grep -vE '^/data/(agents|beads|vaults)(/|$)' \
    | sort -u)"

  for p in $mkdir_paths; do
    # Covered if it is in the list...
    if grep -qxF "$p" <<<"$LIST"; then
      continue
    fi
    # ...or if the site chowns that exact path...
    if grep -qE "chown( -R)? dev:node [^&|;]*${p}( |$|/)" <<<"$ROOT_PHASE"; then
      continue
    fi
    # ...or if an ANCESTOR is chowned RECURSIVELY, which does cover it. E.g.
    # `mkdir -p /data/home/.claude/session-env` is covered by
    # `chown -R dev:node /data/home/.claude`. Only -R counts here: a
    # non-recursive chown of the parent does NOT reach the child.
    covered_by_ancestor=""
    anc="$p"
    while [ "$anc" != "/data" ] && [ "$anc" != "/" ]; do
      anc="$(dirname "$anc")"
      [ "$anc" = "/data" ] && break
      if grep -qE "chown -R dev:node [^&|;]*${anc}( |$)" <<<"$ROOT_PHASE"; then
        covered_by_ancestor=yes
        break
      fi
    done
    [ -n "$covered_by_ancestor" ] && continue
    uncovered="$uncovered $p"
  done

  if [ -n "$uncovered" ]; then
    bad "root-phase mkdir under /data not covered by the sweep list or an inline chown:$uncovered" \
        "add each to HIVE_DATA_ROOT_PHASE_PATHS, or chown it at its creation site (#5369)"
  else
    ok "every root-phase mkdir under /data is swept or chowned at its site"
  fi

  # The two files the root phase WRITES (not mkdirs) under /data. These were
  # the concrete gaps found for #5369: created by `cat >` / `printf >` as root,
  # chmod 644 applied, no chown — so root:root on every boot.
  for f in /data/home/.bashrc /data/home/.profile; do
    if grep -qE "chown dev:node ${f}( |$)" <<<"$ROOT_PHASE"; then
      ok "$f is chowned at its creation site"
    elif grep -qxF "$f" <<<"$LIST"; then
      ok "$f is covered by the sweep list"
    else
      bad "$f is written by root and never handed to dev" \
          "agent shells source it; a root-owned copy is the #5369 class"
    fi
  done
fi

# ── Behavioural: the part that mode-checking could never catch ───────────
#
# Everything above is structure. This is the product property: a root-created
# path, run through the sweep, must be genuinely OPENABLE by uid 1001 — proven
# by really opening it as that uid, which is the syscall the hive binary makes.
echo
echo "=== behavioural: swept paths are readable by the runtime user ==="

if [ "$(id -u)" != "0" ]; then
  hive_test_skip "not root — cannot create root-owned files or drop to another uid" \
       "(this is the case a container lane must run; see #5360/#5369)"
elif ! id -u dev >/dev/null 2>&1; then
  hive_test_skip "no 'dev' account on this host — cannot exercise the drop"
elif ! stat -c '%u' / >/dev/null 2>&1; then
  hive_test_skip "no GNU stat -c on this host — the helpers require it"
else
  SWEEP_FN="$SWEEP"
  ASSERT_FN="$(sed -n '/^hive_assert_runtime_readable() {/,/^}/p' "$ENTRYPOINT")"

  tmpd="$(mktemp -d)"
  trap 'rm -rf "$tmpd"' EXIT
  chmod 755 "$tmpd"

  # Reproduce the failing shape: root creates a directory and a file under it
  # exactly as the root phase does, and never chowns them.
  mkdir -p "$tmpd/home"
  printf 'export SSL_CERT_FILE=/data/proxy-ca.pem\n' > "$tmpd/home/.bashrc"
  chown -R root:root "$tmpd/home"
  chmod 700 "$tmpd/home"          # root-only dir: dev cannot even traverse it
  chmod 600 "$tmpd/home/.bashrc"

  HIVE_RUNTIME_USER="dev"
  HIVE_RUNTIME_GROUP="node"
  export HIVE_RUNTIME_USER HIVE_RUNTIME_GROUP

  # shellcheck disable=SC1090
  eval "$ASSERT_FN"
  # shellcheck disable=SC1090
  eval "$SWEEP_FN"

  # 1. BEFORE the sweep, the runtime user must NOT be able to read it. If this
  #    fails the fixture is wrong and every assertion below is vacuous — the
  #    way a test can pass while proving nothing.
  if su -s /bin/sh dev -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
    bad "fixture invalid: dev could already read the root-owned file before the sweep" \
        "the rest of this block would pass vacuously"
  else
    ok "fixture: the runtime user cannot read the root-created path (the #5369 fault)"
  fi

  # 2. The assertion must NAME that path while it is still broken. This is the
  #    diagnostic #5360 lacked: a silent EACCES from the Go binary versus a
  #    line of output identifying the file.
  assert_out="$(HIVE_RUNTIME_USER=dev hive_assert_runtime_readable "$tmpd/home" "$tmpd/home/.bashrc" 2>&1 || true)"
  if grep -qF "$tmpd/home" <<<"$assert_out"; then
    ok "the assertion names the unreadable path before the privilege drop"
  else
    bad "the assertion did not name the unreadable path" \
        "got: $assert_out"
  fi

  # 3. Run the sweep over that list, then re-check. THE ASSERTION THAT MATTERS:
  #    a real open() as uid 1001, not a stat of the mode bits.
  HIVE_DATA_ROOT_PHASE_PATHS="$tmpd/home
$tmpd/home/.bashrc"
  hive_sweep_root_phase_paths >/dev/null 2>&1

  check "swept directory is owned by the runtime uid" \
    "$RUNTIME_UID" "$(stat -c '%u' "$tmpd/home" 2>/dev/null)"
  check "swept file is owned by the runtime uid" \
    "$RUNTIME_UID" "$(stat -c '%u' "$tmpd/home/.bashrc" 2>/dev/null)"

  if su -s /bin/sh dev -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
    ok "the runtime user can actually read the swept path (real open() as uid 1001)"
  else
    bad "the runtime user STILL cannot read the swept path" \
        "this is #5369 — root-phase writes stay unreadable after the privilege drop"
  fi

  # 4. The sweep must not have bought readability by WIDENING the mode. #5331
  #    exists because a file holding dashboard.auth_token was world-readable;
  #    the fix for #5369 is ownership, never permissions.
  check "the sweep did not widen the file mode" \
    "600" "$(stat -c '%a' "$tmpd/home/.bashrc" 2>/dev/null)"
  check "the sweep did not widen the directory mode" \
    "700" "$(stat -c '%a' "$tmpd/home" 2>/dev/null)"

  if id -u nobody >/dev/null 2>&1; then
    if su -s /bin/sh nobody -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
      bad "an unrelated uid can read the swept file" \
          "readability must come from ownership, not from a widened mode (#5331)"
    else
      ok "an unrelated uid still cannot read the swept file"
    fi
  fi

  # 5. After the sweep the assertion must fall SILENT. A check that warns even
  #    once things are correct is noise, and noise is what makes the real
  #    warning ignorable.
  assert_out2="$(hive_assert_runtime_readable "$tmpd/home" "$tmpd/home/.bashrc" 2>&1 || true)"
  if [ -z "$assert_out2" ]; then
    ok "the assertion is silent once ownership is correct"
  else
    bad "the assertion still warns after a successful sweep" \
        "got: $assert_out2"
  fi

  # 6. FAIL OPEN, not closed (#5368's lesson). When the chown is impossible the
  #    sweep must WARN and continue — never abort the boot, and never leave a
  #    foreign-owned file locked down. A hive that boots degraded and says so
  #    beats one that will not boot.
  HIVE_DATA_ROOT_PHASE_PATHS="/proc/1/mem-does-not-exist
$tmpd/home"
  if hive_sweep_root_phase_paths >/dev/null 2>&1; then
    ok "the sweep fails open (returns success when a path cannot be chowned)"
  else
    bad "the sweep returned non-zero" \
        "under 'set -e' in the entrypoint this aborts the boot — chown must fail open (#5368)"
  fi

  rm -rf "$tmpd"
  trap - EXIT
fi

# ---------------------------------------------------------------------------
# Every interactive-auth CLI credential dir must be group-writable AND repaired
# by the ongoing perm guard.
#
# This is the drift that broke agy sign-in. .gemini was in no list at all, so
# agy created it itself at 2750 — group READ-only. Every sign-in then succeeded
# in-process and evaporated, because the agent UID could not write the token
# file. Nothing reported a permission problem; the operator saw four logins in
# a row appear to fail.
#
# A dir being mkdir'd and chowned is NOT enough, which is why this is separate
# from the sweep check above: the credential file itself is rewritten 0600 by
# the CLI on every refresh (copilot's config.json, agy's oauth token), so a dir
# that is not in the ONGOING guard silently re-locks after the first refresh.
echo
echo "Interactive-auth credential dirs are group-writable and guarded:"
for cred_dir in .claude .copilot .codex .gemini .bob; do
  path="/data/home/$cred_dir"

  if grep -qE "chmod (2770|2775) [^&|;]*${path}( |$|/)" "$ENTRYPOINT"; then
    ok "$cred_dir is created group-writable + setgid"
  else
    bad "$cred_dir has no 'chmod 277x' at its creation site" \
        "an agent UID cannot write it; a CLI sign-in there succeeds in memory and is lost on exit"
  fi

  # The ongoing repair: an inotify watcher on the dir, or the polling
  # slow-cycle sweep. One is enough at the DIRECTORY level; the per-credential
  # block below is what decides whether that guard can actually fire.
  #
  # Four spellings are accepted because #5730 changed how the guards are
  # written without changing what they must cover: the watchers are now
  # dispatched through `hive_guard_forever LABEL DIR ...` (a bare
  # `while inotifywait` exits permanently and silently the first time
  # inotifywait returns non-zero), and the recursive sweep now runs through
  # `hive_fix_tree` over a list rather than one inline `chmod -R`. What this
  # assertion is actually about — every credential dir is covered by SOME
  # ongoing repair — is unchanged, so it accepts both the old and new forms.
  if grep -qE "inotifywait [^|]*${path}/" "$ENTRYPOINT" || \
     grep -qE "hive_guard_forever [a-z]+ ${path}/ " "$ENTRYPOINT" || \
     grep -qE "chmod -R g\+rwX [^&|;]*${path}( |$)" "$ENTRYPOINT" || \
     grep -qE "for _t in [^;]*${path}( |;)" "$ENTRYPOINT"; then
    ok "$cred_dir is repaired by the ongoing perm guard"
  else
    bad "$cred_dir is never re-opened after the CLI rewrites its credential 0600" \
        "add it to the inotify guard or the polling slow-cycle chmod, beside .claude/.codex"
  fi
done

# ---------------------------------------------------------------------------
# Every KNOWN credential FILE must be reachable by the guard that exists for it.
#
# The directory checks above passed for .gemini while the credential they exist
# to protect sat outside the guard's reach (kubestellar/hive#5734). agy keeps
# its OAuth token at .gemini/antigravity-cli/antigravity-oauth-token — one
# level below the watch — and `inotifywait` without -r reports events only for
# entries DIRECTLY inside the watched directory. The guard was not merely
# untested; it was structurally incapable of firing, and it read as protection
# the whole time. Measured on a live hive: sixteen minutes and one token
# rewrite after boot, .claude's inotifywait pid had advanced (its credential is
# at depth 1) while .gemini's was still the boot pid.
#
# So this block asserts on PATHS rather than directories. A credential that
# moves into a subdirectory — exactly what happened — now fails here instead of
# passing silently.
#
# Adding a CLI: put its real credential path in the table. A directory with no
# known credential file (.codex and .bob at the time of writing) is deliberately
# absent: guessing a path would assert nothing, and a wrong guess would assert
# something false.
#
# The loop reads from a here-doc, NOT a pipe: `printf ... | while` would run the
# body in a subshell and every ok/bad below would increment a counter that dies
# with it, so a real failure would print and then be forgotten by the tally.
echo
echo "Known credential files are reachable by their guard:"

# _fn_body FILE NAME — print one shell function's definition from the
# entrypoint, handling both the multi-line form (closing "  }" on its own line)
# and the one-liner form.
_fn_body() {
  awk -v fn="  $2() {" '
    index($0, fn) == 1 { f = 1; print; if ($0 ~ /\}[ \t]*$/) exit; next }
    f { print; if ($0 ~ /^  \}[ \t]*$/) exit }
  ' "$1"
}

# _fast_poll_reach — everything the 5s poll actually repairs, following one
# level of indirection. The poll body calls named helpers (hive_fix_copilot_config
# and friends) rather than spelling every path inline, so a check that read only
# the poll body would report a credential as unprotected when it is repaired on
# every cycle. One level is enough for the current shape and keeps the check
# honest about what it verified.
_fast_poll_reach() {
  _fast="$(_fn_body "$ENTRYPOINT" hive_fix_credentials_fast)"
  printf '%s\n' "$_fast"
  for _callee in $(printf '%s\n' "$_fast" | grep -oE 'hive_fix_[a-z_]+' | sort -u); do
    [ "$_callee" = "hive_fix_credentials_fast" ] && continue
    _fn_body "$ENTRYPOINT" "$_callee"
  done
}
FAST_POLL_REACH="$(_fast_poll_reach)"

while IFS='|' read -r cli cred; do
  [ -n "$cli" ] || continue
  cred_parent="${cred%/*}"

  # 1. Some repair must name the file itself. A guard that only chmods the
  #    directory does not reopen a credential the CLI has just rewritten 0600.
  if grep -qF "$cred" "$ENTRYPOINT"; then
    ok "$cli: the entrypoint names $cred"
  else
    bad "$cli: no repair names $cred" \
        "a directory-level chmod does not reopen a file the CLI rewrote 0600"
  fi

  # 2. The watch must be able to SEE a write to it: either the watch is on the
  #    credential's own parent directory, or an ancestor is watched with -r.
  #    This is the assertion #5734 was missing.
  watched=""
  if grep -qE "hive_guard_forever [a-z]+ ${cred_parent}/ " "$ENTRYPOINT" || \
     grep -qE "inotifywait [^|]*${cred_parent}/( |$)" "$ENTRYPOINT"; then
    watched="direct"
  else
    ancestor="$cred_parent"
    while [ -n "$ancestor" ] && [ "$ancestor" != "/data/home" ] && [ "$ancestor" != "/" ]; do
      ancestor="${ancestor%/*}"
      if grep -qE "hive_guard_forever [a-z]+ ${ancestor}/ [^ ]+ [a-z_]+ -r" "$ENTRYPOINT"; then
        watched="recursive on $ancestor"
        break
      fi
    done
  fi

  if [ -n "$watched" ]; then
    ok "$cli: the inotify guard covers $cred_parent ($watched)"
  else
    bad "$cli: no inotify guard can ever fire for $cred" \
        "the watch is on an ancestor without -r, so a write to this file is invisible to it — watch ${cred_parent}/ directly, or add -r to the ancestor's guard"
  fi

  # 3. The 5s poll must name it too. That is the backstop when inotify is
  #    unavailable (NFS) or its per-user watch limit is exhausted — and after
  #    #5730 it is the arm that cannot die silently.
  if printf '%s\n' "$FAST_POLL_REACH" | grep -qF "$cred"; then
    ok "$cli: the 5s poll repairs $cred"
  else
    bad "$cli: the 5s poll does not repair $cred" \
        "inotify is unreliable on NFS and has a per-user watch limit; the poll is the backstop"
  fi
done <<'CREDENTIAL_PATHS'
copilot|/data/home/.copilot/config.json
claude|/data/home/.claude/.credentials.json
agy|/data/home/.gemini/antigravity-cli/antigravity-oauth-token
CREDENTIAL_PATHS

hive_test_report
