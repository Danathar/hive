#!/usr/bin/env bash
# Contract tests for bin/merge-gate.sh.
# Run: bash bin/test_merge_gate.sh
#
# merge-gate.sh reads /var/run/hive-metrics/actionable.json (written by
# enumerate-actionable.sh) and writes /var/run/hive-metrics/merge-eligible.json
# — the ONE file agents are told to trust for "is this PR safe to merge".
# Every hold/CI/mergeable/author gate that decides whether an agent runs
# `gh pr merge` lives in this one script, and it had no tests.
#
# Like bin/test_enumerate_actionable.sh and bin/test_gh_wrapper_gates.sh, this
# EXECUTES the script rather than grepping it. The script hardcodes
# /var/run/hive-metrics/{actionable,merge-eligible}.json and
# /var/log/kick-agents.log with no env override, so the harness runs a COPY
# with exactly those three paths rewritten to a temp dir, and asserts the
# rewrite landed — a refactor that renames them must fail here loudly, not
# silently run against the real paths. The stub gh serves canned `pr checks`
# TSV and `pr view --json` fixtures keyed by repo/PR number, so the
# IGNORED_CHECKS filter and the eligibility arithmetic are under test, not
# bypassed.
#
# Doctrine (audit 6/7): every exclusion assertion sits next to a positive
# control that IS eligible, so a gate that admits everything cannot pass.
# Hermetic: no network, no sleeps, never touches /var/run, /data or /tmp/hive.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/merge-gate.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0 (#5388
# doctrine). The script under test needs python3; the stub gh needs nothing
# beyond POSIX sh, but the harness's own fixture builders use python3 too.
for dep in python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required"
    exit 1
  fi
done

# Some dev-box python3 shims (asdf/pyenv-style, seen with Homebrew's python3
# symlink under certain toolchain states) hang waiting on stdin when invoked
# non-interactively instead of erroring — invisible in CI (ubuntu-latest's
# python3 is a real interpreter, first on PATH) but would wedge this harness
# locally. Pick the first python3 on PATH that answers `-c "pass" </dev/null`
# inside a hard 3s timeout; put its directory first for every invocation below.
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
# Every bare `python3` call below (fixture builders, jget, prfield, and the
# script copy run via run_gate) must resolve to the same working interpreter,
# not whatever shim happens to be first on the ambient PATH.
export PATH="${PY_PATH}:${PATH}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

RUN_DIR="${WORK}/run"                # stands in for /var/run/hive-metrics
LOG_FILE="${WORK}/kick-agents.log"   # stands in for /var/log/kick-agents.log
SHIM_DIR="${WORK}/shim"
FIX_ROOT="${WORK}/fixtures"
ACTIONABLE="${RUN_DIR}/actionable.json"
OUT="${RUN_DIR}/merge-eligible.json"
GH_LOG="${WORK}/gh.log"
mkdir -p "$RUN_DIR" "$SHIM_DIR" "$FIX_ROOT" "${WORK}/bin"

# ── The path-rewritten copy ──────────────────────────────────────────────────
SCRIPT_COPY="${WORK}/bin/merge-gate.sh"
sed \
  -e "s|/var/run/hive-metrics|${RUN_DIR}|g" \
  -e "s|/var/log/kick-agents.log|${LOG_FILE}|g" \
  "$SCRIPT" >"$SCRIPT_COPY"
if grep -qE '/var/run/hive-metrics|/var/log/kick-agents.log' "$SCRIPT_COPY" \
   || ! grep -q "$RUN_DIR" "$SCRIPT_COPY"; then
  echo "harness-error: path rewrite did not land — the script's hardcoded paths moved; update the sed above"
  exit 1
fi
# The script sources $(dirname "$0")/hive-config.sh first (falling back to
# /usr/local/bin/hive-config.sh, then || true). A no-op stub next to the copy
# means a real hive-config.sh on a dev box is never consulted and PROJECT_*
# come only from the env this harness sets.
: >"${WORK}/bin/hive-config.sh"

# Dev-box convenience only: ubuntu-latest ships GNU date (date -Is works);
# macOS ships BSD date, which rejects -Is. Not a contract under test — on CI
# and in the production containers the real GNU date runs.
if ! date -Is >/dev/null 2>&1; then
  REAL_DATE="$(command -v date)"
  cat >"${SHIM_DIR}/date" <<DATEEOF
#!/bin/sh
[ "\${1:-}" = "-Is" ] && exec "${REAL_DATE}" -u +%Y-%m-%dT%H:%M:%S+00:00
exec "${REAL_DATE}" "\$@"
DATEEOF
  chmod +x "${SHIM_DIR}/date"
fi

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Records every argv to $FAKE_GH_LOG. `pr checks <n> --repo <r>` is served
# from $FAKE_GH_FIXTURES/checks_<repo-with-underscores>_<n>.tsv as raw TSV
# text (gh pr checks is NOT JSON). `pr view <n> --repo <r> --json ...` is
# served from $FAKE_GH_FIXTURES/view_<repo>_<n>.json. Missing fixtures mean
# "gh call failed" (empty stdout, non-zero exit) so the error-handling paths
# are reachable without a dedicated flag file.
cat >"${SHIM_DIR}/gh" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_GH_LOG}"
cmd="${1:-}"
case "$cmd" in
  pr)
    sub="${2:-}"
    case "$sub" in
      checks)
        num="${3:-}"
        repo=""
        shift 3
        while [ $# -gt 0 ]; do
          case "$1" in
            --repo) repo="$2"; shift 2 ;;
            *) shift ;;
          esac
        done
        key="checks_$(printf '%s' "$repo" | tr '/' '_')_${num}"
        f="${FAKE_GH_FIXTURES}/${key}.tsv"
        [ -f "$f" ] || { echo "stub gh: no checks fixture for ${key}" >&2; exit 1; }
        cat "$f"
        # gh pr checks exits non-zero whenever any check is failing/pending —
        # mirror that so the caller's `|| true` path is exercised too.
        grep -qiE '\bfail\b|\bpending\b' "$f" && exit 1
        exit 0
        ;;
      view)
        num="${3:-}"
        repo=""
        shift 3
        while [ $# -gt 0 ]; do
          case "$1" in
            --repo) repo="$2"; shift 2 ;;
            *) shift ;;
          esac
        done
        key="view_$(printf '%s' "$repo" | tr '/' '_')_${num}"
        f="${FAKE_GH_FIXTURES}/${key}.json"
        [ -f "$f" ] || { echo '{}'; exit 1; }
        cat "$f"
        exit 0
        ;;
      *) exit 0 ;;
    esac
    ;;
  *) exit 0 ;;
esac
STUBEOF
chmod +x "${SHIM_DIR}/gh"

# ── Fixtures ─────────────────────────────────────────────────────────────────
# One repo, several PRs exercising each gate independently plus one PR that
# combines two block reasons (labels test the reasons array, not just the
# eligible/not_ready split).
mkchecks() { # mkchecks <file> "<name>\t<status>" ...
  local f="$1"; shift
  : >"$f"
  for line in "$@"; do printf '%b\n' "$line" >>"$f"; done
}
mkview() { # mkview <file> <author> <draft> <mergeable> <reviewDecision> <labels-json>
  local f="$1"; shift
  printf '{"title":"t","author":{"login":"%s"},"isDraft":%s,"mergeable":"%s","reviewDecision":"%s","labels":%s}\n' \
    "$1" "$2" "$3" "$4" "$5" >"$f"
}

REPO="acme/primary"
RKEY="acme_primary"

# 401: AI-authored, CI green, mergeable, not held -> ELIGIBLE (positive control)
mkchecks "${FIX_ROOT}/checks_${RKEY}_401.tsv" "unit-tests\tpass\t1m\turl" "lint\tpass\t10s\turl"
mkview "${FIX_ROOT}/view_${RKEY}_401.json" "hive-bot" "false" "MERGEABLE" "" "[]"

# 402: community-authored, CI green, mergeable, APPROVED -> ELIGIBLE (positive control)
mkchecks "${FIX_ROOT}/checks_${RKEY}_402.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_402.json" "somedev" "false" "MERGEABLE" "APPROVED" "[]"

# 403: community-authored, CI green, mergeable, NOT approved -> not_ready (author gate)
mkchecks "${FIX_ROOT}/checks_${RKEY}_403.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_403.json" "somedev" "false" "MERGEABLE" "" "[]"

# 404: AI-authored but a required check FAILS -> not_ready (ci gate)
mkchecks "${FIX_ROOT}/checks_${RKEY}_404.tsv" "unit-tests\tfail\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_404.json" "hive-bot" "false" "MERGEABLE" "" "[]"

# 405: AI-authored, only an IGNORED check fails -> ELIGIBLE (IGNORED_CHECKS filter)
mkchecks "${FIX_ROOT}/checks_${RKEY}_405.tsv" "unit-tests\tpass\t1m\turl" "Playwright e2e\tfail\t2m\turl" "tide\tpending\t0s\turl"
mkview "${FIX_ROOT}/view_${RKEY}_405.json" "hive-bot" "false" "MERGEABLE" "" "[]"

# 406: AI-authored, a check is "skipping" (conditional job) -> treated as pass -> ELIGIBLE
mkchecks "${FIX_ROOT}/checks_${RKEY}_406.tsv" "unit-tests\tpass\t1m\turl" "windows-only\tskipping\t0s\turl"
mkview "${FIX_ROOT}/view_${RKEY}_406.json" "hive-bot" "false" "MERGEABLE" "" "[]"

# 407: AI-authored, CI green, but CONFLICTING -> not_ready (mergeable gate)
mkchecks "${FIX_ROOT}/checks_${RKEY}_407.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_407.json" "hive-bot" "false" "CONFLICTING" "" "[]"

# 408: AI-authored, CI green, mergeable=UNKNOWN (GitHub still computing) -> ELIGIBLE
mkchecks "${FIX_ROOT}/checks_${RKEY}_408.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_408.json" "hive-bot" "false" "UNKNOWN" "" "[]"

# 409: AI-authored, CI green, mergeable, but carries a `hold` label -> not_ready (deterministic hold-gate)
mkchecks "${FIX_ROOT}/checks_${RKEY}_409.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_409.json" "hive-bot" "false" "MERGEABLE" "" '[{"name":"hold"}]'

# 410: community-authored, APPROVED, CI green, mergeable, but `do-not-merge` label -> not_ready
#      (hold-gate wins over approval — held is checked before is_ai/approved)
mkchecks "${FIX_ROOT}/checks_${RKEY}_410.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_410.json" "somedev" "false" "MERGEABLE" "APPROVED" '[{"name":"do-not-merge"}]'

# 411: gh pr checks returns nothing at all -> not_ready, status=error (fetch-failure gate)
# (no checks fixture written for 411 — stub exits 1 with a stderr message and
#  the script's `checks=$(... 2>/dev/null) || true` makes $checks empty)

# 412: AI-authored via PROJECT_AI_AUTHOR env override, CI green, mergeable -> ELIGIBLE
mkchecks "${FIX_ROOT}/checks_${RKEY}_412.tsv" "unit-tests\tpass\t1m\turl"
mkview "${FIX_ROOT}/view_${RKEY}_412.json" "custom-bot" "false" "MERGEABLE" "" "[]"

# ── Runner ───────────────────────────────────────────────────────────────────
run_gate() { # run_gate [VAR=value...]
  : >"$GH_LOG"
  env "$@" \
    PATH="${SHIM_DIR}:${PATH}" \
    FAKE_GH_LOG="$GH_LOG" \
    FAKE_GH_FIXTURES="$FIX_ROOT" \
    bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo $?
}
jget() { python3 -c "
import json,sys
try:
    d=json.load(open('$OUT'))
except Exception as e:
    print('JSON-ERROR:'+str(e)); sys.exit(0)
print($1)
" 2>/dev/null; }

prfield() { # prfield <list> <number> <field>
  python3 -c "
import json
d=json.load(open('$OUT'))
for p in d.get('$1', []):
    if p.get('number') == $2:
        v = p.get('$3')
        print(v if not isinstance(v, list) else ','.join(v))
        break
else:
    print('__NOTFOUND__')
"
}

assert_eq() { # assert_eq <label> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

echo "=== merge-gate.sh contract tests ==="

# ── 0. No actionable.json at all: SKIP, exit 0, no output written ───────────
echo "-- no actionable.json --"
rc="$(run_gate PROJECT_AI_AUTHOR=hive-bot)"
assert_eq "no actionable.json: exits 0" "$rc" "0"
[ -e "$OUT" ] && fail "no actionable.json: merge-eligible.json NOT written" || pass "no actionable.json: merge-eligible.json NOT written"
grep -q 'SKIP' "$LOG_FILE" && pass "no actionable.json: SKIP logged" || fail "no actionable.json: SKIP logged"

# ── 1. actionable.json with an empty PR list: exits 0, empty eligible doc ───
echo "-- empty PR list --"
printf '{"prs":{"items":[]}}\n' >"$ACTIONABLE"
rc="$(run_gate PROJECT_AI_AUTHOR=hive-bot)"
assert_eq "empty PR list: exits 0" "$rc" "0"
assert_eq "empty PR list: publishes an empty (not missing) document" \
  "$(jget "'{}/{}/{}'.format(d['count'], len(d['merge_eligible']), len(d['not_ready']))")" "0/0/0"
assert_eq "empty PR list: count is 0" "$(jget "d['count']")" "0"
assert_eq "empty PR list: no gh calls made" "$(wc -l <"$GH_LOG" | tr -d ' ')" "0"

# ── 2. Full run against all fixture PRs ──────────────────────────────────────
echo "-- full gate run --"
python3 -c "
import json
prs = [{'repo':'$REPO','number':n} for n in [401,402,403,404,405,406,407,408,409,410,411,412]]
print(json.dumps({'prs':{'items':prs}}))
" >"$ACTIONABLE"
rc="$(run_gate PROJECT_AI_AUTHOR=hive-bot)"
assert_eq "full run exits 0" "$rc" "0"
if ! python3 -c "import json; json.load(open('$OUT'))" 2>/dev/null; then
  echo "harness-error: merge-eligible.json is not valid JSON; stderr was:"; cat "${WORK}/stderr"
  exit 1
fi

ELIGIBLE="$(jget "','.join(str(p['number']) for p in d['merge_eligible'])")"
NOTREADY="$(jget "','.join(str(p['number']) for p in d['not_ready'])")"

echo "-- eligible / not_ready membership --"
for n in 401 402 405 406 408; do
  if [[ ",${ELIGIBLE}," == *",${n},"* ]]; then pass "PR #${n} is merge-eligible"; else fail "PR #${n} is merge-eligible" "eligible list: $ELIGIBLE"; fi
done
for excl in "403:community author, not approved" "404:required check failing" "407:not mergeable (CONFLICTING)" "409:hold label" "410:do-not-merge label overrides approval" "411:gh pr checks fetch failed"; do
  n="${excl%%:*}"; why="${excl#*:}"
  if [[ ",${NOTREADY}," == *",${n},"* ]]; then pass "PR #${n} is not_ready: ${why}"; else fail "PR #${n} is not_ready: ${why}" "not_ready list: $NOTREADY"; fi
done
assert_eq "count == number of eligible PRs" "$(jget "d['count']")" "$(jget "len(d['merge_eligible'])")"
assert_eq "eligible + not_ready covers every PR in actionable.json" \
  "$(( $(jget "len(d['merge_eligible'])") + $(jget "len(d['not_ready'])") ))" "12"

echo "-- IGNORED_CHECKS filter (tide, Playwright, attribute, Storybook, Visual , Verify build after merge) --"
assert_eq "PR #405: Playwright/tide failures are ignored, only unit-tests counts" "$(prfield merge_eligible 405 status)" "pass"

echo "-- 'skipping' status normalized to pass --"
assert_eq "PR #406: a skipping check does not block eligibility" "$(prfield merge_eligible 406 status)" "pass"

echo "-- block_reasons content on not_ready PRs --"
assert_eq "PR #403 block_reasons cites author, not held/ci/mergeable" "$(prfield not_ready 403 block_reasons)" "author=somedev (not AI, not approved)"
assert_eq "PR #404 block_reasons cites ci status" "$(prfield not_ready 404 block_reasons)" "ci=fail"
assert_eq "PR #407 block_reasons cites mergeable=CONFLICTING" "$(prfield not_ready 407 block_reasons)" "mergeable=CONFLICTING"
assert_eq "PR #409 block_reasons cites held (hold label present)" "$(prfield not_ready 409 block_reasons)" "held (hold label present)"
assert_eq "PR #410 block_reasons cites held even though reviewDecision=APPROVED (hold-gate wins)" "$(prfield not_ready 410 block_reasons)" "held (hold label present)"
assert_eq "PR #411 status is 'error' when gh pr checks fetch fails" "$(prfield not_ready 411 status)" "error"

echo "-- ai_authored / held / ci_pass / approved flags on the record --"
assert_eq "PR #401 ai_authored=True (default AI_AUTHORS set)" "$(prfield merge_eligible 401 ai_authored)" "True"
assert_eq "PR #402 approved=True (community + APPROVED review)" "$(prfield merge_eligible 402 approved)" "True"
assert_eq "PR #409 held=True" "$(prfield not_ready 409 held)" "True"

echo "-- PROJECT_AI_AUTHOR env override extends AI_AUTHORS --"
python3 -c "
import json
print(json.dumps({'prs':{'items':[{'repo':'$REPO','number':412}]}}))
" >"$ACTIONABLE"
rc="$(run_gate PROJECT_AI_AUTHOR=custom-bot)"
assert_eq "custom AI author run exits 0" "$rc" "0"
assert_eq "PR #412 (author=custom-bot) is eligible only when PROJECT_AI_AUTHOR=custom-bot is set" \
  "$(jget "','.join(str(p['number']) for p in d['merge_eligible'])")" "412"

echo "-- POSITIVE CONTROL: without PROJECT_AI_AUTHOR override, custom-bot is NOT auto-trusted --"
rc="$(run_gate)"
assert_eq "no PROJECT_AI_AUTHOR: run still exits 0" "$rc" "0"
assert_eq "PR #412 (author=custom-bot) is NOT eligible without the override (not in the built-in AI_AUTHORS set)" \
  "$(jget "','.join(str(p['number']) for p in d['merge_eligible'])")" ""

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
