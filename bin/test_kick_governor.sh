#!/usr/bin/env bash
# Contract tests for bin/kick-governor.sh.
# Run: bash bin/test_kick_governor.sh
#
# kick-governor.sh is the adaptive kick timer: every 15 minutes it measures
# the actionable backlog, ladders scanner/ci-maintainer/architect/outreach
# through SURGE/BUSY/QUIET/IDLE cadences, downgrades models under token-budget
# pressure, and decides which agent gets kicked (or paused) this tick. It is
# 923 lines, called from bin/hive.sh, and had no tests.
#
# Like bin/test_enumerate_actionable.sh and bin/test_gh_wrapper_gates.sh this
# EXECUTES the script rather than grepping it. The script hardcodes
# /var/run/kick-governor, /var/log/kick-governor.log, /var/log/kick-audit.jsonl,
# /var/run/hive-metrics and /etc/hive with no env override, so the harness runs
# a COPY with exactly those paths rewritten to a temp dir (and asserts the
# rewrite landed — a refactor that renames them must fail here loudly, not
# silently run the tests against the real paths).
#
# Doctrine (audit 6/7): every exclusion/pause assertion sits next to a
# positive control that DOES fire, so a governor that kicks nothing (or kicks
# everything) cannot pass. Hermetic: no network, no sleeps (the buffer-clear
# sleeps and any script sleep are stubbed), never touches /var/run, /data or
# /tmp/hive. tmux is stubbed to "no session" unconditionally — none of the
# contracts pinned here exercise the stuck-buffer/tmux block, and CI images
# are not guaranteed to ship tmux.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/kick-governor.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0 (#5388
# doctrine). ubuntu-latest ships python3; the script itself needs it for
# budget math and queue measurement.
for dep in python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required (script under test needs it for budget/queue math)"
    exit 1
  fi
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STATE_DIR_T="${WORK}/state"          # stands in for /var/run/kick-governor
METRICS_DIR_T="${WORK}/metrics"      # stands in for /var/run/hive-metrics
ETC_HIVE_T="${WORK}/etc-hive"        # stands in for /etc/hive
LOG_FILE_T="${WORK}/kick-governor.log"
AUDIT_LOG_T="${WORK}/kick-audit.jsonl"
SHIM_DIR="${WORK}/shim"
mkdir -p "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T" "$SHIM_DIR" "${WORK}/bin" "${WORK}/home"

# ── The path-rewritten copies ────────────────────────────────────────────────
# RAW: paths rewritten only. hive_is_paused is left undefined, exactly as the
# real script leaves it — used once, to pin the crash (contract 1).
# MAIN: paths rewritten, PLUS a hive_is_paused() shim injected right after
# `set -euo pipefail`. Every other contract runs against this copy; without
# the shim, every call to _is_agent_paused (6 call sites) aborts the script
# under set -e (verified: bash reports "hive_is_paused: command not found",
# exit 127) before any of the mode/budget/pin logic below it ever runs.
sed_rewrite() {
  sed \
    -e "s|/var/run/kick-governor|${STATE_DIR_T}|g" \
    -e "s|/var/log/kick-governor.log|${LOG_FILE_T}|g" \
    -e "s|/var/log/kick-audit.jsonl|${AUDIT_LOG_T}|g" \
    -e "s|/var/run/hive-metrics|${METRICS_DIR_T}|g" \
    -e "s|/etc/hive|${ETC_HIVE_T}|g" \
    "$SCRIPT"
}

SCRIPT_RAW="${WORK}/bin/kick-governor.raw.sh"
sed_rewrite >"$SCRIPT_RAW"

SCRIPT_COPY="${WORK}/bin/kick-governor.sh"
sed_rewrite | sed "/^set -euo pipefail\$/a\\
hive_is_paused() {\\
  local agent=\"\${1:?agent name required}\"\\
  [[ -f \"${STATE_DIR_T}/paused_\${agent}\" ]] || \\\\\\
  [[ -f \"${STATE_DIR_T}/operator_paused_\${agent}\" ]] || \\\\\\
  [[ -f \"${STATE_DIR_T}/cadence_paused_\${agent}\" ]]\\
}" >"$SCRIPT_COPY"

for copy in "$SCRIPT_RAW" "$SCRIPT_COPY"; do
  if grep -qE '/var/run/kick-governor|/var/log/kick-governor\.log|/var/log/kick-audit\.jsonl|/var/run/hive-metrics|/etc/hive' "$copy"; then
    echo "harness-error: path rewrite did not land in ${copy} — the script's hardcoded paths moved; update sed_rewrite() above"
    exit 1
  fi
done
if ! grep -q 'hive_is_paused()' "$SCRIPT_COPY"; then
  echo "harness-error: hive_is_paused shim injection did not land — 'set -euo pipefail' line moved; update the injection anchor"
  exit 1
fi
chmod +x "$SCRIPT_RAW" "$SCRIPT_COPY"

# The script sources ../config/backends.conf relative to its own dir for
# normalize_model_for_backend/model_tier — carry the real one along so the
# budget/model-selection contracts run against real logic, not a stub.
mkdir -p "${WORK}/config"
cp "${REPO_ROOT}/config/backends.conf" "${WORK}/config/backends.conf"

# ── Stub tools ───────────────────────────────────────────────────────────────
# sleep: the buffer-clear path sleeps 1s+2s; a local dev box may also lack
# passwordless sudo (used once, to touch a lock file on full pin).
printf '#!/bin/sh\nexit 0\n' >"${SHIM_DIR}/sleep"
printf '#!/bin/sh\nexec "$@"\n' >"${SHIM_DIR}/sudo"
chmod +x "${SHIM_DIR}/sleep" "${SHIM_DIR}/sudo"

# tmux: unconditionally "no session" — none of the pinned contracts exercise
# the stuck-buffer block, and CI images are not guaranteed to ship tmux.
cat >"${SHIM_DIR}/tmux" <<'TMUXEOF'
#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  *) exit 1 ;;
esac
TMUXEOF
chmod +x "${SHIM_DIR}/tmux"

# GNU date is required (-d, -Is). macOS ships BSD date; ubuntu-latest (CI)
# ships GNU date natively and this shim is not used there. Implemented as
# pure /bin/sh + BSD `date -v`/`-j -f`, not a python subprocess wrapper —
# spawning a real subprocess from python hung indefinitely in one dev
# sandbox during calibration; native BSD date has no such issue and this
# path never runs on CI regardless.
if ! date -d "+1 seconds" >/dev/null 2>&1 || ! date -Is >/dev/null 2>&1; then
  REAL_DATE="$(command -v date)"
  cat >"${SHIM_DIR}/date" <<DATEEOF
#!/bin/sh
REAL_DATE="${REAL_DATE}"
case "\$1" in
  -Is)
    exec "\$REAL_DATE" '+%Y-%m-%dT%H:%M:%S%z'
    ;;
  -d)
    spec="\$2"; shift 2
    case "\$spec" in
      +*)
        n="\${spec%% *}"; n="\${n#+}"
        exec "\$REAL_DATE" -v"+\${n}S" "\$@"
        ;;
      *)
        "\$REAL_DATE" -j -f '%Y-%m-%dT%H:%M:%S' "\${spec%%[+Z]*}" "\$@" 2>/dev/null || echo 0
        ;;
    esac
    ;;
  *)
    exec "\$REAL_DATE" "\$@"
    ;;
esac
DATEEOF
  chmod +x "${SHIM_DIR}/date"
fi

# ── notify.sh stub ───────────────────────────────────────────────────────────
# The real notify() (bin/notify.sh) shells out to curl for ntfy/Slack/Discord.
# ntfy()/notify() are called unconditionally on every mode change and kick
# (governor.sh only sources notify.sh, never defines notify() itself), so an
# absent NOTIFY_LIB is not "notifications are skipped" — it is
# "notify: command not found" under set -e. No network in this harness: stub
# notify() as a no-op that just logs the call was made.
NOTIFY_LIB_STUB="${WORK}/bin/notify-stub.sh"
cat >"$NOTIFY_LIB_STUB" <<'NOTIFYEOF'
notify() { echo "notify:$1" >> "${NOTIFY_LOG:-/dev/null}"; }
NOTIFYEOF
NOTIFY_LOG="${WORK}/notify.log"

# ── kick-agents.sh stub ──────────────────────────────────────────────────────
# KICK_SCRIPT is env-overridable in the real script — records every
# invocation to KICK_LOG and exits with $KICK_EXIT (default 0).
KICK_LOG="${WORK}/kicks.log"
KICK_SCRIPT_STUB="${WORK}/bin/kick-agents-stub.sh"
cat >"$KICK_SCRIPT_STUB" <<'KICKEOF'
#!/bin/sh
echo "kicked:$1" >> "${KICK_LOG}"
echo "kick-agents stub output for $1"
exit "${KICK_EXIT:-0}"
KICKEOF
chmod +x "$KICK_SCRIPT_STUB"

# ── Runner ───────────────────────────────────────────────────────────────────
# run_gov [VAR=value...] — runs the (shimmed) main copy, echoes exit code.
run_gov() {
  : >"${WORK}/stdout"; : >"${WORK}/stderr"
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    KICK_LOG="$KICK_LOG" \
    KICK_SCRIPT="$KICK_SCRIPT_STUB" \
    NOTIFY_LIB="$NOTIFY_LIB_STUB" \
    NOTIFY_LOG="$NOTIFY_LOG" \
    OUTCOME_TRACKER="/nonexistent-outcome-tracker.sh" \
    NOUS_SNAPSHOTS_DIR="${WORK}/nous-snapshots" \
    bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo $?
}
run_gov_raw() {
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    KICK_LOG="$KICK_LOG" \
    KICK_SCRIPT="$KICK_SCRIPT_STUB" \
    NOTIFY_LIB="$NOTIFY_LIB_STUB" \
    NOTIFY_LOG="$NOTIFY_LOG" \
    OUTCOME_TRACKER="/nonexistent-outcome-tracker.sh" \
    NOUS_SNAPSHOTS_DIR="${WORK}/nous-snapshots" \
    bash "$SCRIPT_RAW" >"${WORK}/stdout-raw" 2>"${WORK}/stderr-raw"
  echo $?
}

reset_state() {
  rm -rf "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T" "$KICK_LOG"
  mkdir -p "$STATE_DIR_T" "$METRICS_DIR_T" "$ETC_HIVE_T"
  : >"$LOG_FILE_T"
}

write_actionable() { # write_actionable <n-issues> <n-prs>
  local ni="$1" np="$2" i items_i="[]" items_p="[]" arr=()
  for ((i = 0; i < ni; i++)); do arr+=("{\"repo\":\"acme/primary\",\"number\":${i}}"); done
  items_i=$(printf '%s\n' "${arr[@]:-}" | paste -sd, -); [ "$ni" -eq 0 ] && items_i=""
  arr=()
  for ((i = 0; i < np; i++)); do arr+=("{\"repo\":\"acme/primary\",\"number\":${i}}"); done
  items_p=$(printf '%s\n' "${arr[@]:-}" | paste -sd, -); [ "$np" -eq 0 ] && items_p=""
  printf '{"issues":{"items":[%s]},"prs":{"items":[%s]}}\n' "$items_i" "$items_p" \
    >"${METRICS_DIR_T}/actionable.json"
}

assert_eq() { # assert_eq <label> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

# grep -c always prints a count (even "0") but exits 1 on no match, so a bare
# `|| echo 0` fallback double-prints on that path; only a genuinely missing
# file prints nothing. Normalize both to exactly one line.
kick_count() {
  [ -f "$KICK_LOG" ] || { echo 0; return; }
  grep -c "^kicked:$1\$" "$KICK_LOG" 2>/dev/null || true
}

# Matches bin/hive.sh's own AGENTS_ENABLED default (${AGENTS_ENABLED:-"supervisor
# scanner ci-maintainer architect outreach"}, used in 4 places) — see contract
# 7b below for what happens when an operator's AGENTS_ENABLED omits supervisor.
BASE_ENV=(AGENTS_ENABLED="supervisor scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary")

echo "=== kick-governor.sh contract tests ==="

# ── 0. Path rewrite sanity: a bare run reaches GOVERNOR DONE ────────────────
reset_state
write_actionable 0 0
rc="$(run_gov "${BASE_ENV[@]}")"
assert_eq "sanity: shimmed copy runs to completion (rewrite + hive_is_paused shim both hold)" "$rc" "0"
grep -q "GOVERNOR DONE" "$LOG_FILE_T" && pass "sanity: GOVERNOR DONE reached" || fail "sanity: GOVERNOR DONE reached" "log: $(cat "$LOG_FILE_T")"

# ── 1. hive_is_paused is never sourced — PINNED BUG (fails OPEN, not closed) ─
# kick-governor.sh calls _is_agent_paused() -> hive_is_paused() at 6 call
# sites (maybe_kick, optimize_model_assignment, the model-cadence writer, the
# buffer-clear loop, the stale-status loop) but only ever sources
# backends.conf and /etc/hive/governor*.env — never hive-config.sh, the file
# that DEFINES hive_is_paused. No systemd unit for kick-governor.timer exists
# in-repo either, so nothing is shown to source hive-config.sh into the
# governor's shell before it runs.
#
# This does NOT crash the script: every one of the 6 call sites is inside an
# `if _is_agent_paused ...; then` or `_is_agent_paused ... && continue` — both
# are set -e EXEMPT contexts (a command's failure inside an if/&&/|| condition
# never triggers set -e). So "hive_is_paused: command not found" (exit 127)
# is silently treated as "agent is NOT paused" at every call site. The
# governor runs to GOVERNOR DONE and kicks normally — it just never honours
# ANY pause file (dashboard pause, operator pause, cadence-zero pause) for as
# long as hive_is_paused stays undefined. That is fail-OPEN on a safety
# control, the opposite of what an operator hitting "pause" would expect.
# Pinned so a fix (sourcing hive-config.sh, or inlining the check) flips this
# deliberately; see the PR body for the finding.
echo "-- pinned bug: hive_is_paused undefined makes every pause check fail OPEN --"
reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/paused_scanner"   # an operator pause the unmodified script should honour
rc="$(run_gov_raw AGENTS_ENABLED="scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary")"
assert_eq "[pinned bug] unmodified script still exits 0 (set -e does NOT fire — the call sits inside an if/&&)" "$rc" "0"
grep -q 'hive_is_paused.*not found\|command not found' "${WORK}/stderr-raw" \
  && pass "[pinned bug] stderr carries 'hive_is_paused: command not found' on every call site, unhandled" \
  || fail "[pinned bug] stderr carries 'hive_is_paused: command not found' on every call site, unhandled" "stderr: $(cat "${WORK}/stderr-raw")"
grep -q "GOVERNOR DONE" "${WORK}/stderr-raw" \
  && pass "[pinned bug] the script still reaches GOVERNOR DONE (does not abort)" \
  || fail "[pinned bug] the script still reaches GOVERNOR DONE (does not abort)" "stderr: $(cat "${WORK}/stderr-raw")"
grep -q '^kicked:scanner$' "$KICK_LOG" \
  && pass "[pinned bug] the paused_scanner marker is IGNORED — scanner is kicked anyway" \
  || fail "[pinned bug] the paused_scanner marker is IGNORED — scanner is kicked anyway" "kicks: $(cat "$KICK_LOG" 2>/dev/null)"

# ── 2. AGENTS_ENABLED unset fails closed and loud, not silent ───────────────
echo "-- AGENTS_ENABLED unset: fail-closed under set -u --"
reset_state
write_actionable 0 0
rc="$(run_gov HIVE_REPOS="acme/primary")"   # no AGENTS_ENABLED in env at all
assert_eq "unset AGENTS_ENABLED: exits non-zero, not a silent no-op" "$( [ "$rc" != "0" ] && echo nonzero || echo 0 )" "nonzero"
grep -q 'AGENTS_ENABLED.*unbound variable\|unbound variable' "${WORK}/stderr" \
  && pass "unset AGENTS_ENABLED: set -u reports unbound variable" \
  || fail "unset AGENTS_ENABLED: set -u reports unbound variable" "stderr: $(cat "${WORK}/stderr")"
assert_eq "unset AGENTS_ENABLED: POSITIVE CONTROL — a set value runs clean" "$(run_gov "${BASE_ENV[@]}")" "0"

# ── 3. Mode laddering thresholds (SURGE>20, BUSY>10, QUIET>2, else IDLE) ────
echo "-- mode laddering --"
for case in "0:idle" "2:idle" "3:quiet" "10:quiet" "11:busy" "20:busy" "21:surge" "50:surge"; do
  n="${case%%:*}"; want="${case#*:}"
  reset_state
  write_actionable "$n" 0
  run_gov "${BASE_ENV[@]}" >/dev/null
  got="$(cat "${STATE_DIR_T}/mode" 2>/dev/null || echo MISSING)"
  assert_eq "queue=${n} issues -> mode=${want} (boundary)" "$got" "$want"
done

# ── 4. Cadence lookup: hyphenated agent name -> underscored env var ─────────
echo "-- cadence lookup (ci-maintainer hyphen->underscore) --"
reset_state
write_actionable 25 0   # surge
run_gov "${BASE_ENV[@]}" CADENCE_CI_MAINTAINER_SURGE_SEC=777 >/dev/null
assert_eq "an EXPLICITLY set CADENCE_CI_MAINTAINER_*_SEC IS honoured (hyphen->underscore mapping works)" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "12min"
reset_state
write_actionable 25 0
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "POSITIVE CONTROL: scanner is never paused by mode (cadence=15min in every mode)" \
  "$(cat "${STATE_DIR_T}/cadence_scanner" 2>/dev/null)" "15min"
assert_eq "surge mode: architect cadence=0 renders as off" "$(cat "${STATE_DIR_T}/cadence_architect" 2>/dev/null)" "off"

# DOCUMENTS THE CURRENT STATE (BUG — see the PR body): the header comment and
# `priority_agents=(scanner ci-maintainer)` (line 497) both name the agent
# ci-maintainer, and AGENTS_ENABLED's own default in bin/hive.sh is
# "supervisor scanner ci-maintainer architect outreach" — ci-maintainer IS the
# real agent name. But every CADENCE_*_SEC and MODEL_*_* default in this file
# is written for an agent called "reviewer" instead (CADENCE_REVIEWER_*,
# MODEL_*_REVIEWER). get_cadence()/get_model_selection() build the lookup key
# from the AGENT name, so an operator who never overrides
# CADENCE_CI_MAINTAINER_*_SEC gets cadence=0 (mode-paused, forever, in every
# mode) for ci-maintainer, and get_model_selection() silently falls through to
# its hardcoded "copilot:claude-sonnet-4-6" default in every mode too — the
# entire ci-maintainer cadence ladder and model ladder in the header comment
# (lines 8-34) are unreachable dead configuration. Pinned so a fix (renaming
# the REVIEWER defaults to CI_MAINTAINER, or renaming the agent) flips this
# deliberately.
reset_state
write_actionable 0 0   # idle: CADENCE_REVIEWER_IDLE_SEC defaults to 900 (15min) if it were consulted
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "[pinned bug] ci-maintainer's cadence is 0/off in idle mode — CADENCE_REVIEWER_IDLE_SEC (900) is never consulted" \
  "$(cat "${STATE_DIR_T}/cadence_ci-maintainer" 2>/dev/null)" "off"
assert_eq "[pinned bug] ci-maintainer is never kicked as a result, even though it is the highest-priority non-scanner agent" \
  "$(kick_count ci-maintainer)" "0"
assert_eq "[pinned bug] ci-maintainer's model falls through to the hardcoded default, not MODEL_IDLE_REVIEWER" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_ci-maintainer" 2>/dev/null | cut -d= -f2):$(grep '^MODEL=' "${STATE_DIR_T}/model_ci-maintainer" 2>/dev/null | cut -d= -f2)" \
  "copilot:claude-sonnet-4-6"

# ── 5. Dashboard pause / operator-resume / cadence=0 handling (maybe_kick) ──
echo "-- pause handling --"
reset_state
write_actionable 0 0   # idle: every agent has cadence>0
touch "${STATE_DIR_T}/paused_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "dashboard-paused agent is not kicked" "$(kick_count scanner)" "0"
assert_eq "POSITIVE CONTROL: an unpaused agent with cadence>0 and never-kicked-before IS kicked" \
  "$(kick_count outreach)" "1"
grep -q '"agent":"scanner","action":"SKIP","reason":"dashboard-paused"' "$AUDIT_LOG_T" \
  && pass "SKIP audit record carries reason=dashboard-paused" \
  || fail "SKIP audit record carries reason=dashboard-paused" "audit log: $(cat "$AUDIT_LOG_T")"

reset_state
write_actionable 25 0   # surge: architect+outreach cadence=0 (paused by mode)
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "cadence=0 (mode-paused) agent is not kicked" "$(kick_count architect)" "0"
[ -f "${STATE_DIR_T}/cadence_paused_architect" ] \
  && pass "cadence=0 writes cadence_paused_<agent> marker" \
  || fail "cadence=0 writes cadence_paused_<agent> marker"

reset_state
write_actionable 25 0
touch "${STATE_DIR_T}/operator_resumed_architect"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "operator-resume overrides cadence=0: does not re-pause (marker NOT (re)written)" \
  "$( [ -f "${STATE_DIR_T}/cadence_paused_architect" ] && echo present || echo absent )" "absent"
assert_eq "operator-resume with cadence=0 still does not KICK (only stops re-pausing)" \
  "$(kick_count architect)" "0"

reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/paused_scanner" "${STATE_DIR_T}/was_paused_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null   # still paused this tick -> stays skipped, was_paused untouched by the un-pause branch
reset_state
write_actionable 0 0
touch "${STATE_DIR_T}/was_paused_scanner"   # paused LAST tick, not paused now
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "unpause transition: agent is kicked immediately, ignoring its normal cadence" \
  "$(kick_count scanner)" "1"
assert_eq "unpause transition: was_paused marker is cleared" \
  "$( [ -f "${STATE_DIR_T}/was_paused_scanner" ] && echo present || echo absent )" "absent"

# ── 6. Model lock / pin precedence ───────────────────────────────────────────
echo "-- model lock / pin precedence --"
reset_state
write_actionable 0 0   # idle mode -> MODEL_IDLE_SCANNER default copilot:claude-sonnet-4-6
touch "${STATE_DIR_T}/model_lock_scanner"
printf 'BACKEND=claude\nMODEL=claude-opus-4-6\n' >"${STATE_DIR_T}/model_scanner"
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "model_lock_<agent> freezes the model file (manual override wins)" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"

reset_state
write_actionable 0 0
mkdir -p "$ETC_HIVE_T"
printf 'AGENT_CLI_PINNED=true\n' >"${ETC_HIVE_T}/scanner.env"
run_gov "${BASE_ENV[@]}" >/dev/null
[ -f "${STATE_DIR_T}/model_lock_scanner" ] \
  && pass "AGENT_CLI_PINNED=true creates the lock file (full pin escalates to a standing lock)" \
  || fail "AGENT_CLI_PINNED=true creates the lock file"
assert_eq "POSITIVE CONTROL: an agent with no pin/lock DOES get a model file written" \
  "$( [ -f "${STATE_DIR_T}/model_ci-maintainer" ] && echo written || echo missing )" "written"

# ── 7. Budget pressure ladder (optimize_model_assignment) ───────────────────
echo "-- budget pressure ladder --"
reset_state
write_actionable 25 0   # surge -> MODEL_SURGE_ARCHITECT defaults to claude:claude-opus-4-6
mkdir -p "$METRICS_DIR_T"
# used=90% of a 100-token budget, 0 hours elapsed history -> avg_hourly ~ used
# (hours_elapsed floors at 1), so projected == used == 90% for a same-tick read.
printf '{"weekly":{"billableTokens":90},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
run_gov "${BASE_ENV[@]}" TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85 >/dev/null
# outreach's own MODEL_*_OUTREACH default is copilot in every mode, so it can
# never actually be observed being downgraded FROM claude; architect's surge
# default (claude:claude-opus-4-6) is the one that exercises this branch.
assert_eq "budget >85% safety: non-priority agent (architect) downgraded to copilot" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_architect" | cut -d= -f2)" "copilot"
assert_eq "budget >85% safety: reason recorded as budget_downgrade" \
  "$(grep '^REASON=' "${STATE_DIR_T}/model_architect" | cut -d= -f2)" "budget_downgrade"
assert_eq "POSITIVE CONTROL: priority agent (scanner) is NOT downgraded at 90% (<95% opus/sonnet ladder threshold)" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"
# scanner defaults to copilot in idle mode already, so use surge (claude) to see the >95 ladder.
reset_state
write_actionable 25 0   # surge -> MODEL_SURGE_SCANNER=claude:claude-sonnet-4-6
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":96},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
run_gov "${BASE_ENV[@]}" TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85 >/dev/null
# DOCUMENTS THE CURRENT STATE (BUG — see the PR body): the sonnet->haiku
# downgrade is a literal string substitution, `${model/sonnet/haiku}`, applied
# to "claude-sonnet-4-6" — it swaps the tier word but leaves the version
# suffix untouched, producing "claude-haiku-4-6". Every OTHER haiku default in
# this file (MODEL_*_SUPERVISOR, MODEL_QUIET_SCANNER) and config/backends.conf's
# own model_tier() both use "claude-haiku-4-5" — 4-6 is not a haiku version
# that exists. A budget-critical downgrade (the exact moment this code path
# exists to protect against runaway spend) hands the CLI a model name that
# does not exist. Pinned so a fix (mapping tier AND version together, e.g.
# via model_tier()/a lookup table) flips this deliberately.
assert_eq "[pinned bug] budget >95%: sonnet->haiku downgrade keeps the SONNET version suffix (4-6), not haiku's real 4-5" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2):$(grep '^MODEL=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" \
  "claude:claude-haiku-4-6"

reset_state
write_actionable 25 0
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":99},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
touch "${METRICS_DIR_T}/budget_ignore"
run_gov "${BASE_ENV[@]}" TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85 >/dev/null
assert_eq "BUDGET_IGNORE_FLAG bypasses every downgrade even above 99%" \
  "$(grep '^BACKEND=' "${STATE_DIR_T}/model_scanner" | cut -d= -f2)" "claude"

# DOCUMENTS THE CURRENT STATE (BUG — see the PR body): the budget-downgrade
# loop at line ~514 is `for agent in outreach architect supervisor` —
# hardcoded, not derived from AGENTS_ENABLED/agents[@]. The `assignments`
# associative array (declare -A) is populated ONLY for agents actually in
# AGENTS_ENABLED (line ~504). bin/hive.sh's own default AGENTS_ENABLED
# includes supervisor, but it is an operator-configurable env var — nothing
# stops an operator from running without it. If they do, and the token budget
# then crosses TOKEN_BUDGET_SAFETY_PCT, `${assignments[supervisor]}` reads an
# unset associative-array key under `set -u` and the WHOLE
# optimize_model_assignment() call aborts (bare statement, not inside an
# if/&&, so set -e DOES fire here — unlike the hive_is_paused case above).
# That happens before maybe_kick ever runs (line 776 vs 919), so ONE missing
# agent name in this hardcoded list means NO agent gets kicked that cycle,
# the moment the budget crosses the safety threshold. Pinned so a fix
# (deriving the loop from agents[@], or guarding the lookup) flips this
# deliberately.
echo "-- pinned bug: budget pressure crashes if AGENTS_ENABLED omits 'supervisor' --"
reset_state
write_actionable 0 0
mkdir -p "$METRICS_DIR_T"
printf '{"weekly":{"billableTokens":90},"hourlyBurnRate":{"billable":0}}\n' \
  >"${METRICS_DIR_T}/tokens.json"
rc="$(run_gov AGENTS_ENABLED="scanner ci-maintainer architect outreach" HIVE_REPOS="acme/primary" \
  TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85)"
assert_eq "[pinned bug] governor exits non-zero when budget pressure hits and supervisor is absent" \
  "$( [ "$rc" != "0" ] && echo nonzero || echo 0 )" "nonzero"
grep -q "assignments\[.*\]: unbound variable" "${WORK}/stderr" \
  && pass "[pinned bug] the documented crash signature: assignments[\$agent]: unbound variable" \
  || fail "[pinned bug] the documented crash signature: assignments[\$agent]: unbound variable" "stderr: $(cat "${WORK}/stderr")"
grep -q "GOVERNOR DONE" "${WORK}/stderr" \
  && fail "[pinned bug] GOVERNOR DONE is never reached — no agent is kicked this cycle" \
  || pass "[pinned bug] GOVERNOR DONE is never reached — no agent is kicked this cycle"
assert_eq "[pinned bug] POSITIVE CONTROL: the same scenario WITH supervisor present does not crash" \
  "$(run_gov "${BASE_ENV[@]}" TOKEN_BUDGET_WEEKLY=100 TOKEN_BUDGET_SAFETY_PCT=85)" "0"

# ── 8. What is written where — the file contract other tooling reads ────────
echo "-- state files written --"
reset_state
write_actionable 5 3   # quiet: 8 total (>2, <=10)
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "mode file" "$(cat "${STATE_DIR_T}/mode")" "quiet"
assert_eq "queue_issues file" "$(cat "${STATE_DIR_T}/queue_issues")" "5"
assert_eq "queue_prs file" "$(cat "${STATE_DIR_T}/queue_prs")" "3"
assert_eq "queue_depth file (issues+prs)" "$(cat "${STATE_DIR_T}/queue_depth")" "8"
# busyness_pct is issues-only/threshold*100 (mode is determined from
# get_queue_depth(), which returns measure_queue's total_i — issues only, NOT
# the combined issues+prs total written to the queue_depth FILE): 5 issues *
# 100 / BUSY_THRESHOLD_ISSUES(10) = 50, even though 8 total items are queued.
assert_eq "busyness_pct file is issues-only/threshold*100 (NOT the combined total in the queue_depth file)" \
  "$(cat "${STATE_DIR_T}/busyness_pct")" "50"

# ── 9. get_queue_depth fallback: actionable.json missing -> cached depth ────
echo "-- queue-measurement fallback on missing actionable.json --"
reset_state
write_actionable 25 0
run_gov "${BASE_ENV[@]}" >/dev/null   # seed queue_depth=25 (surge) in state
rm -f "${METRICS_DIR_T}/actionable.json"   # now simulate the collector being down
rm -f "${STATE_DIR_T}/cadence_paused_"*    # avoid stale pause markers muddying this run
run_gov "${BASE_ENV[@]}" >/dev/null
assert_eq "measure_queue falls back to per-repo caches (none present) -> total=0 -> idle, NOT the stale surge depth" \
  "$(cat "${STATE_DIR_T}/mode")" "idle"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
