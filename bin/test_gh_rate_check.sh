#!/usr/bin/env bash
# Contract tests for bin/gh-rate-check.sh.
# Run: bash bin/test_gh_rate_check.sh
#
# gh-rate-check.sh scans agent tmux panes for GitHub API rate-limit messages
# and decides which agents to PAUSE (by touching a governor flag file) and
# for how long. A pattern regression here either stops pausing agents during
# a real rate limit (burns the fleet's quota faster) or pauses agents on a
# false positive (Claude/Copilot CLI usage-limit text that looks similar but
# is a DIFFERENT limit — the script's own header calls this out explicitly).
# It had no tests.
#
# Like bin/test_merge_gate.sh and bin/test_enumerate_actionable.sh, this
# EXECUTES the script rather than grepping it. The script hardcodes
# /var/run/hive-metrics/{gh_rate_limits.json,rate_pullback/}, /var/run/kick-
# governor and /var/log/kick-agents.log with no env override, so the harness
# runs a COPY with those four paths rewritten to a temp dir, and asserts the
# rewrite landed. GH_BIN, TMUX_BIN, NTFY_SERVER, NTFY_TOPIC and GOVERNOR_ENV
# ARE overridable via env, so the harness uses the real overrides instead of
# rewriting those.
#
# Doctrine (audit 6/7): every exclusion assertion sits next to a positive
# control that DOES fire, so a matcher that never fires cannot pass.
# Hermetic: no network (curl targets 127.0.0.1 and is expected to fail
# silently, exactly as production does when ntfy is unreachable), no real
# sleeps, never touches /var/run, /data or /tmp/hive.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/gh-rate-check.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

for dep in python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required"
    exit 1
  fi
done

# Some dev-box python3 shims hang waiting on stdin when invoked non-
# interactively instead of erroring — invisible in CI (ubuntu-latest's
# python3 is a real interpreter, first on PATH) but would wedge this harness
# locally. Pick the first python3 on PATH that answers `-c "pass" </dev/null`
# inside a hard 3s timeout; put its directory first for every invocation.
PY_PATH=""
IFS=':' read -r -a _path_dirs <<<"$PATH"
for d in "${_path_dirs[@]}"; do
  [ -x "${d}/python3" ] || continue
  "${d}/python3" -c "pass" </dev/null >/dev/null 2>&1 &
  cand_pid=$!
  waited=0
  while kill -0 "$cand_pid" 2>/dev/null && [ "$waited" -lt 30 ]; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$cand_pid" 2>/dev/null; then
    kill -9 "$cand_pid" 2>/dev/null
    continue
  fi
  wait "$cand_pid" 2>/dev/null && { PY_PATH="$d"; break; }
done
if [ -z "$PY_PATH" ]; then
  echo "harness-error: no working non-interactive python3 found on PATH"
  exit 1
fi
export PATH="${PY_PATH}:${PATH}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

RUN_DIR="${WORK}/run"                      # stands in for /var/run/hive-metrics
GOV_DIR="${WORK}/governor"                 # stands in for /var/run/kick-governor
LOG_FILE="${WORK}/kick-agents.log"         # stands in for /var/log/kick-agents.log
SHIM_DIR="${WORK}/shim"
ETC_HIVE="${WORK}/etc-hive"                # stands in for /etc/hive
METRICS="${RUN_DIR}/gh_rate_limits.json"
PULLBACK_DIR="${RUN_DIR}/rate_pullback"
mkdir -p "$RUN_DIR" "$GOV_DIR" "$SHIM_DIR" "${WORK}/bin" "$ETC_HIVE" "$PULLBACK_DIR"

# ── The path-rewritten copy ──────────────────────────────────────────────────
SCRIPT_COPY="${WORK}/bin/gh-rate-check.sh"
sed \
  -e "s|/var/run/hive-metrics|${RUN_DIR}|g" \
  -e "s|/var/run/kick-governor|${GOV_DIR}|g" \
  -e "s|/var/log/kick-agents.log|${LOG_FILE}|g" \
  -e "s|/etc/hive/|${ETC_HIVE}/|g" \
  "$SCRIPT" >"$SCRIPT_COPY"
if grep -qE '/var/run/hive-metrics|/var/run/kick-governor|/var/log/kick-agents\.log|/etc/hive/' "$SCRIPT_COPY" \
   || ! grep -q "$RUN_DIR" "$SCRIPT_COPY" \
   || ! grep -q "$GOV_DIR" "$SCRIPT_COPY"; then
  echo "harness-error: path rewrite did not land — the script's hardcoded paths moved; update the sed above"
  exit 1
fi

# Dev-box convenience only: ubuntu-latest ships GNU date; macOS BSD date
# rejects -Is. Not a contract under test.
if ! date -Is >/dev/null 2>&1; then
  REAL_DATE="$(command -v date)"
  cat >"${SHIM_DIR}/date" <<DATEEOF
#!/bin/sh
[ "\${1:-}" = "-Is" ] && exec "${REAL_DATE}" -u +%Y-%m-%dT%H:%M:%S+00:00
exec "${REAL_DATE}" "\$@"
DATEEOF
  chmod +x "${SHIM_DIR}/date"
fi

# ── Stub gh (rate_limit endpoint only) ───────────────────────────────────────
# `gh api rate_limit --jq FILTER` is served by evaluating FILTER against
# $FAKE_RATE_LIMIT_JSON with real jq, so the --jq filters in get_api_reset_
# epoch and the recovery check are under test, not bypassed.
GH_STUB="${SHIM_DIR}/gh"
cat >"$GH_STUB" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_GH_LOG:-/dev/null}"
if [ "${1:-}" = "api" ] && [ "${2:-}" = "rate_limit" ]; then
  jqf=""
  shift 2
  while [ $# -gt 0 ]; do
    case "$1" in --jq) jqf="$2"; shift 2 ;; *) shift ;; esac
  done
  if [ -n "$jqf" ]; then
    jq -r "$jqf" "$FAKE_RATE_LIMIT_JSON"
  else
    cat "$FAKE_RATE_LIMIT_JSON"
  fi
  exit 0
fi
exit 0
STUBEOF
chmod +x "$GH_STUB"

for dep in jq; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required (stub gh needs it to honour --jq)"
    exit 1
  fi
done

# ── Stub tmux ─────────────────────────────────────────────────────────────────
# `has-session -t <name>` succeeds only for sessions listed in
# $FAKE_TMUX_SESSIONS (space-separated). `capture-pane -t <name> -p ...`
# prints $FAKE_TMUX_PANES/<name>.txt, or nothing if absent.
TMUX_STUB="${SHIM_DIR}/tmux"
cat >"$TMUX_STUB" <<'STUBEOF'
#!/usr/bin/env bash
case "${1:-}" in
  has-session)
    target=""
    shift
    while [ $# -gt 0 ]; do case "$1" in -t) target="$2"; shift 2 ;; *) shift ;; esac; done
    for s in ${FAKE_TMUX_SESSIONS:-}; do
      [ "$s" = "$target" ] && exit 0
    done
    exit 1
    ;;
  capture-pane)
    target=""
    shift
    while [ $# -gt 0 ]; do case "$1" in -t) target="$2"; shift 2 ;; *) shift ;; esac; done
    f="${FAKE_TMUX_PANES}/${target}.txt"
    [ -f "$f" ] && cat "$f"
    exit 0
    ;;
  *) exit 0 ;;
esac
STUBEOF
chmod +x "$TMUX_STUB"

# ── Stub curl (ntfy) ─────────────────────────────────────────────────────────
# Records every ntfy POST (title header + body) to $FAKE_NTFY_LOG so
# assertions can check WHICH notifications fired without any network call.
CURL_STUB="${SHIM_DIR}/curl"
cat >"$CURL_STUB" <<'STUBEOF'
#!/usr/bin/env bash
title=""
body=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-H" ] && [[ "$a" == Title:* ]]; then title="${a#Title: }"; fi
  if [ "$prev" = "-d" ]; then body="$a"; fi
  prev="$a"
done
printf 'TITLE=%s BODY=%s\n' "$title" "$body" >> "${FAKE_NTFY_LOG:-/dev/null}"
exit 0
STUBEOF
chmod +x "$CURL_STUB"

# ── Runner ───────────────────────────────────────────────────────────────────
GH_LOG="${WORK}/gh.log"
NTFY_LOG="${WORK}/ntfy.log"
run_check() { # run_check [VAR=value...]
  : >"$GH_LOG"; : >"$NTFY_LOG"
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    HOME="${WORK}/home" \
    GH_BIN="$GH_STUB" \
    TMUX_BIN="$TMUX_STUB" \
    FAKE_GH_LOG="$GH_LOG" \
    FAKE_NTFY_LOG="$NTFY_LOG" \
    FAKE_RATE_LIMIT_JSON="${RATE_LIMIT_JSON:-${WORK}/rate_default.json}" \
    FAKE_TMUX_SESSIONS="${TMUX_SESSIONS:-}" \
    FAKE_TMUX_PANES="${TMUX_PANES:-${WORK}/panes_empty}" \
    bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo $?
}
jget() { python3 -c "
import json,sys
try:
    d=json.load(open('$METRICS'))
except Exception as e:
    print('JSON-ERROR:'+str(e)); sys.exit(0)
print($1)
" 2>/dev/null
}
assert_eq() { # assert_eq <label> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

mkdir -p "${WORK}/panes_empty"
NOW_EPOCH_FILE="${WORK}/now_epoch"
python3 -c "import time; print(int(time.time()))" > "$NOW_EPOCH_FILE"
NOW_EPOCH="$(cat "$NOW_EPOCH_FILE")"

# Default: STILL-EXHAUSTED rate limit (remaining=0). Phase 2.5 clears every
# active alert (and its pullback) the moment the API shows remaining > 0 —
# that is the recovery contract under test in section 6, and it would also
# silently erase every alert this harness sets up for detection/dedup/
# exclusion/operator-paused scenarios if the default fixture looked healthy.
# Only the recovery tests below swap in a healthy fixture deliberately.
printf '{"rate":{"remaining":0,"limit":5000},"resources":{"core":{"reset":%d},"graphql":{"reset":%d}}}\n' \
  "$((NOW_EPOCH + 3600))" "$((NOW_EPOCH + 3600))" > "${WORK}/rate_default.json"

echo "=== gh-rate-check.sh contract tests ==="

# ── 0. No tmux sessions running: exits 0, empty alerts, empty pullbacks ─────
echo "-- no agent sessions running --"
rc="$(TMUX_SESSIONS="" run_check)"
assert_eq "no sessions: exits 0" "$rc" "0"
if ! python3 -c "import json; json.load(open('$METRICS'))" 2>/dev/null; then
  echo "harness-error: gh_rate_limits.json is not valid JSON; stderr was:"; cat "${WORK}/stderr"
  exit 1
fi
assert_eq "no sessions: alerts is empty" "$(jget "len(d['alerts'])")" "0"
assert_eq "no sessions: pullbacks is empty" "$(jget "len(d['pullbacks'])")" "0"
assert_eq "no sessions: no agent gets paused" "$(ls "$GOV_DIR" 2>/dev/null | wc -l | tr -d ' ')" "0"

# ── 1. Detection: GH rate-limit text triggers an alert + pullback ──────────
echo "-- detection: GH rate limit text in scanner's pane --"
PANES1="${WORK}/panes1"; mkdir -p "$PANES1"
cat >"${PANES1}/scanner.txt" <<'EOF'
some earlier output
Error: API rate limit exceeded for installation ID 123.
more output after
EOF
# Give ci-maintainer, architect, outreach real sessions too (same default CLI
# detection: no /etc/hive/*.env means get_agent_cli returns "unknown" for all
# four, so a pullback on cli=unknown pauses the other three).
: >"${PANES1}/ci-maintainer.txt"
: >"${PANES1}/architect.txt"
: >"${PANES1}/outreach.txt"
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES1" run_check)"
assert_eq "detection run exits 0" "$rc" "0"
assert_eq "one alert recorded for scanner" "$(jget "len(d['alerts'])")" "1"
assert_eq "alert is attributed to the scanner agent" "$(jget "d['alerts'][0]['agent']")" "scanner"
assert_eq "alert message captures the matched line (truncated, trimmed)" \
  "$(jget "d['alerts'][0]['message']")" "Error: API rate limit exceeded for installation ID 123."
assert_eq "scanner itself is NOT paused (let it finish its cycle)" \
  "$( [ -f "${GOV_DIR}/paused_scanner" ] && echo paused || echo not-paused )" "not-paused"
for other in ci-maintainer architect outreach; do
  assert_eq "same-CLI agent ${other} IS paused by the pullback" \
    "$( [ -f "${GOV_DIR}/paused_${other}" ] && echo paused || echo not-paused )" "paused"
done
assert_eq "exactly one pullback state file is written (cli=unknown)" \
  "$(ls "$PULLBACK_DIR"/pullback_*.json 2>/dev/null | wc -l | tr -d ' ')" "1"
PBFILE="$(ls "$PULLBACK_DIR"/pullback_*.json)"
assert_eq "pullback records the three agents it paused" \
  "$(python3 -c "import json; print(sorted(json.load(open('$PBFILE'))['paused_agents']))")" \
  "$(python3 -c "print(sorted(['ci-maintainer','architect','outreach']))")"
assert_eq "pullback ntfy fires naming the paused agents" \
  "$(grep -c 'TITLE=Rate Limit Pullback' "$NTFY_LOG")" "1"
assert_eq "per-hit ntfy also fires for the rate limit itself" \
  "$(grep -c 'TITLE=GH Rate Limit: scanner' "$NTFY_LOG")" "1"

# ── 2. Dedup: a second run with the SAME pane text does not re-alert ───────
echo "-- dedup: already-alerted agent is not re-added --"
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES1" run_check)"
assert_eq "second run exits 0" "$rc" "0"
assert_eq "still exactly one alert (deduped, not doubled)" "$(jget "len(d['alerts'])")" "1"
assert_eq "second run: no NEW pullback file created (existing one still active)" \
  "$(ls "$PULLBACK_DIR"/pullback_*.json 2>/dev/null | wc -l | tr -d ' ')" "1"
assert_eq "second run: no second pullback ntfy (pullback already exists)" \
  "$(grep -c 'TITLE=Rate Limit Pullback' "$NTFY_LOG")" "0"

# ── 3. CLI-usage-limit text does NOT trigger a GH rate-limit alert ─────────
echo "-- POSITIVE/NEGATIVE: CLI usage-limit text is excluded, GH text still matches --"
rm -rf "$PULLBACK_DIR" "$GOV_DIR"/paused_* "$METRICS"
mkdir -p "$PULLBACK_DIR"
PANES2="${WORK}/panes2"; mkdir -p "$PANES2"
cat >"${PANES2}/scanner.txt" <<'EOF'
You're out of extra usage credits, resets 3pm
EOF
: >"${PANES2}/ci-maintainer.txt"; : >"${PANES2}/architect.txt"; : >"${PANES2}/outreach.txt"
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES2" run_check)"
assert_eq "CLI-usage-limit-only run exits 0" "$rc" "0"
assert_eq "NEGATIVE CONTROL: CLI usage-limit text raises NO GH rate-limit alert" "$(jget "len(d['alerts'])")" "0"

cat >"${PANES2}/scanner.txt" <<'EOF'
You're out of extra usage credits, resets 3pm
secondary rate limit hit while retrying
EOF
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES2" run_check)"
assert_eq "mixed-text run exits 0" "$rc" "0"
assert_eq "POSITIVE CONTROL: a genuine GH pattern (secondary rate limit) still fires alongside excluded CLI text" \
  "$(jget "len(d['alerts'])")" "1"
assert_eq "matched line is the GH one, not the excluded CLI line" \
  "$(jget "d['alerts'][0]['message']")" "secondary rate limit hit while retrying"

# ── 4. operator-paused agents are never touched by pullback OR its expiry ──
echo "-- operator-paused agents are protected from pullback pause AND resume --"
rm -rf "$PULLBACK_DIR" "$GOV_DIR"/paused_* "$GOV_DIR"/operator_paused_* "$METRICS"
mkdir -p "$PULLBACK_DIR"
touch "${GOV_DIR}/operator_paused_architect"
PANES3="${WORK}/panes3"; mkdir -p "$PANES3"
cat >"${PANES3}/scanner.txt" <<'EOF'
abuse detection mechanism triggered, please retry later
EOF
: >"${PANES3}/ci-maintainer.txt"; : >"${PANES3}/architect.txt"; : >"${PANES3}/outreach.txt"
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES3" run_check)"
assert_eq "run exits 0" "$rc" "0"
assert_eq "operator-paused architect is NOT touched (no paused_architect governor flag added)" \
  "$( [ -f "${GOV_DIR}/paused_architect" ] && echo touched || echo untouched )" "untouched"
PBFILE3="$(ls "$PULLBACK_DIR"/pullback_*.json)"
assert_eq "architect recorded as already_paused, not paused_agents" \
  "$(python3 -c "
import json
d=json.load(open('$PBFILE3'))
print('already' if 'architect' in d['already_paused'] and 'architect' not in d['paused_agents'] else 'wrong')
")" "already"
for other in ci-maintainer outreach; do
  assert_eq "POSITIVE CONTROL: ${other} (not operator-paused) IS paused by the pullback" \
    "$( [ -f "${GOV_DIR}/paused_${other}" ] && echo paused || echo not-paused )" "paused"
done

echo "-- expiry: operator-paused agent stays paused after pullback expiry, others resume --"
# Force the pullback to look expired.
python3 -c "
import json
d = json.load(open('$PBFILE3'))
d['expiry_epoch'] = 1
json.dump(d, open('$PBFILE3', 'w'))
"
rc="$(TMUX_SESSIONS="" run_check)"
assert_eq "expiry run exits 0" "$rc" "0"
assert_eq "operator-paused architect is STILL paused after expiry (never auto-resumed)" \
  "$( [ -f "${GOV_DIR}/operator_paused_architect" ] && echo still-operator-paused || echo lost )" "still-operator-paused"
for other in ci-maintainer outreach; do
  assert_eq "governor-paused ${other} IS resumed when the pullback expires" \
    "$( [ -f "${GOV_DIR}/paused_${other}" ] && echo still-paused || echo resumed )" "resumed"
done
assert_eq "expired pullback file is removed" "$(ls "$PULLBACK_DIR"/pullback_*.json 2>/dev/null | wc -l | tr -d ' ')" "0"
assert_eq "expiry sends a Pullback Expired ntfy" "$(grep -c 'TITLE=Rate Limit Pullback Expired' "$NTFY_LOG")" "1"

# ── 5. TTL-based alert expiry (no api_reset_epoch on the alert) ────────────
echo "-- TTL-based alert expiry --"
rm -rf "$PULLBACK_DIR" "$GOV_DIR"/paused_* "$METRICS"; mkdir -p "$PULLBACK_DIR"
python3 -c "
import json
alert = {'agent':'scanner','cli':'unknown','detected_at':'x','detected_epoch': $NOW_EPOCH - 1000,
          'message':'old','ttl_seconds':900,'pullback_seconds':900,'api_reset_epoch':0}
json.dump({'alerts':[alert]}, open('$METRICS','w'))
"
rc="$(TMUX_SESSIONS="" run_check)"
assert_eq "TTL expiry run exits 0" "$rc" "0"
assert_eq "an alert older than its TTL (no api_reset) is pruned" "$(jget "len(d['alerts'])")" "0"

echo "-- POSITIVE CONTROL: an alert within its TTL survives pruning --"
python3 -c "
import json
alert = {'agent':'scanner','cli':'unknown','detected_at':'x','detected_epoch': $NOW_EPOCH - 10,
          'message':'fresh','ttl_seconds':900,'pullback_seconds':900,'api_reset_epoch':0}
json.dump({'alerts':[alert]}, open('$METRICS','w'))
"
rc="$(TMUX_SESSIONS="" run_check)"
assert_eq "fresh-alert run exits 0" "$rc" "0"
assert_eq "an alert within TTL is kept (not pruned)" "$(jget "len(d['alerts'])")" "1"

echo "-- api_reset_epoch-based expiry takes priority over TTL --"
python3 -c "
import json
# TTL says this should still be fresh (detected 10s ago, ttl 900s), but the
# API reset time is already in the past — the reset time must win.
alert = {'agent':'scanner','cli':'unknown','detected_at':'x','detected_epoch': $NOW_EPOCH - 10,
          'message':'reset-passed','ttl_seconds':900,'pullback_seconds':900,'api_reset_epoch': $NOW_EPOCH - 1}
json.dump({'alerts':[alert]}, open('$METRICS','w'))
"
rc="$(TMUX_SESSIONS="" run_check)"
assert_eq "reset-based expiry run exits 0" "$rc" "0"
assert_eq "an alert whose api_reset_epoch has passed is pruned even though its TTL has not elapsed" \
  "$(jget "len(d['alerts'])")" "0"

# ── 6. Recovery: remaining > 0 clears all alerts AND active pullbacks ──────
echo "-- recovery clears alerts and unpauses (except operator-paused) --"
rm -rf "$PULLBACK_DIR" "$GOV_DIR"/paused_* "$GOV_DIR"/operator_paused_* "$METRICS"; mkdir -p "$PULLBACK_DIR"
python3 -c "
import json
alert = {'agent':'scanner','cli':'unknown','detected_at':'x','detected_epoch': $NOW_EPOCH,
          'message':'still active by ttl','ttl_seconds':900,'pullback_seconds':900,'api_reset_epoch':0}
json.dump({'alerts':[alert]}, open('$METRICS','w'))
"
touch "${GOV_DIR}/paused_ci-maintainer"
touch "${GOV_DIR}/operator_paused_architect"
python3 -c "
import json
state = {'cli':'unknown','triggered_by':'scanner','triggered_at':'x','expiry_epoch': $NOW_EPOCH + 3600,
         'paused_agents':['ci-maintainer','architect'],'already_paused':[],'api_reset_epoch': $NOW_EPOCH + 3600}
json.dump(state, open('$PULLBACK_DIR/pullback_unknown.json','w'))
"
RATE_LIMIT_JSON="${WORK}/rate_recovered.json"
printf '{"rate":{"remaining":100,"limit":5000},"resources":{"core":{"reset":%d},"graphql":{"reset":%d}}}\n' \
  "$((NOW_EPOCH + 3600))" "$((NOW_EPOCH + 3600))" > "$RATE_LIMIT_JSON"
rc="$(TMUX_SESSIONS="" RATE_LIMIT_JSON="$RATE_LIMIT_JSON" run_check)"
assert_eq "recovery run exits 0" "$rc" "0"
assert_eq "recovery clears the active alert" "$(jget "len(d['alerts'])")" "0"
assert_eq "recovery resumes the governor-paused agent" \
  "$( [ -f "${GOV_DIR}/paused_ci-maintainer" ] && echo still-paused || echo resumed )" "resumed"
assert_eq "recovery does NOT touch the operator-paused agent's own flag" \
  "$( [ -f "${GOV_DIR}/operator_paused_architect" ] && echo intact || echo removed )" "intact"
assert_eq "recovery removes the now-stale pullback file" \
  "$(ls "$PULLBACK_DIR"/pullback_*.json 2>/dev/null | wc -l | tr -d ' ')" "0"

echo "-- POSITIVE CONTROL: remaining <= 0 does NOT trigger recovery --"
rm -rf "$PULLBACK_DIR" "$METRICS"; mkdir -p "$PULLBACK_DIR"
python3 -c "
import json
alert = {'agent':'scanner','cli':'unknown','detected_at':'x','detected_epoch': $NOW_EPOCH,
          'message':'still active','ttl_seconds':900,'pullback_seconds':900,'api_reset_epoch':0}
json.dump({'alerts':[alert]}, open('$METRICS','w'))
"
RATE_LIMIT_EXHAUSTED="${WORK}/rate_exhausted.json"
printf '{"rate":{"remaining":0,"limit":5000},"resources":{"core":{"reset":%d},"graphql":{"reset":%d}}}\n' \
  "$((NOW_EPOCH + 3600))" "$((NOW_EPOCH + 3600))" > "$RATE_LIMIT_EXHAUSTED"
rc="$(TMUX_SESSIONS="" RATE_LIMIT_JSON="$RATE_LIMIT_EXHAUSTED" run_check)"
assert_eq "still-exhausted run exits 0" "$rc" "0"
assert_eq "alert count stays 1 (no false recovery while remaining=0)" "$(jget "len(d['alerts'])")" "1"

# ── 7. GOVERNOR_ENV pattern overrides are honoured ──────────────────────────
echo "-- GOVERNOR_ENV pattern override --"
rm -rf "$PULLBACK_DIR" "$GOV_DIR"/paused_* "$METRICS"; mkdir -p "$PULLBACK_DIR"
CUSTOM_ENV="${WORK}/governor.env"
cat >"$CUSTOM_ENV" <<'EOF'
SENSING_GH_RATE_PATTERNS='TOTALLY-CUSTOM-MARKER'
EOF
PANES4="${WORK}/panes4"; mkdir -p "$PANES4"
cat >"${PANES4}/scanner.txt" <<'EOF'
API rate limit exceeded (would match the DEFAULT pattern, not the override)
EOF
: >"${PANES4}/ci-maintainer.txt"; : >"${PANES4}/architect.txt"; : >"${PANES4}/outreach.txt"
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES4" GOVERNOR_ENV="$CUSTOM_ENV" run_check)"
assert_eq "custom-pattern run exits 0" "$rc" "0"
assert_eq "with the pattern overridden to a marker that isn't present, the default-pattern text does NOT alert" \
  "$(jget "len(d['alerts'])")" "0"

cat >"${PANES4}/scanner.txt" <<'EOF'
here it is: TOTALLY-CUSTOM-MARKER in the pane
EOF
rc="$(TMUX_SESSIONS="scanner ci-maintainer architect outreach" TMUX_PANES="$PANES4" GOVERNOR_ENV="$CUSTOM_ENV" run_check)"
assert_eq "POSITIVE CONTROL: the overridden pattern DOES alert when its marker is present" \
  "$(jget "len(d['alerts'])")" "1"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
