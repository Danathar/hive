#!/usr/bin/env bash
set -u -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/bin/hive-baseline-check.sh"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

PASS=0
FAIL=0
MOCK_BIN="$TMP_ROOT/bin"
mkdir -p "$MOCK_BIN"

cat >"$MOCK_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"$MOCK_GH_LOG"
if [[ "${MOCK_GH_FAIL:-}" == "1" ]]; then
  exit 1
fi
case "$1:$2" in
  api:repos/acme/widgets)
    printf '{"default_branch":"trunk"}\n'
    ;;
  api:repos/acme/widgets/commits/trunk/check-runs?per_page=100)
    cat "$MOCK_BASE_RUNS"
    ;;
  pr:list)
    cat "$MOCK_PRS"
    ;;
  *)
    echo "unexpected gh invocation: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$MOCK_BIN/gh"

export MOCK_GH_LOG="$TMP_ROOT/gh.log"
export MOCK_BASE_RUNS="$TMP_ROOT/base.json"
export MOCK_PRS="$TMP_ROOT/prs.json"

run_helper() {
  set +e
  OUTPUT="$(PATH="$MOCK_BIN:$PATH" "$SCRIPT" "$@" 2>&1)"
  STATUS=$?
  set -e
}

check_status() {
  local want="$1" label="$2"
  if [[ "$STATUS" -eq "$want" ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (want exit $want, got $STATUS; output: $OUTPUT)"
    FAIL=$((FAIL + 1))
  fi
}

check_output() {
  local needle="$1" label="$2"
  if [[ "$OUTPUT" == *"$needle"* ]]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (missing '$needle'; output: $OUTPUT)"
    FAIL=$((FAIL + 1))
  fi
}

check_file_contains() {
  local file="$1" needle="$2" label="$3"
  if grep -Fq -- "$needle" "$file"; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label ($file is missing '$needle')"
    FAIL=$((FAIL + 1))
  fi
}

write_base() {
  printf '%s\n' "$1" >"$MOCK_BASE_RUNS"
}

write_prs() {
  printf '%s\n' "$1" >"$MOCK_PRS"
}

echo "=== hive-baseline-check.sh tests ==="

write_base '{"check_runs":[{"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[]'
run_helper acme/widgets build
check_status 0 "red default-branch check is shared"
check_output 'default branch "trunk" is red' "human output names the real default branch"

write_base '{"check_runs":[{"name":"build","status":"completed","conclusion":"success","completed_at":"2026-08-30T01:00:00Z"}]}'
write_prs '[
  {"number":11,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":12,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"TIMED_OUT"}]},
  {"number":13,"statusCheckRollup":[{"context":"build","state":"ERROR"}]},
  {"number":14,"statusCheckRollup":[{"name":"build docs","status":"COMPLETED","conclusion":"FAILURE"}]}
]'
run_helper acme/widgets build --json
check_status 0 "three sibling PR failures are shared"
check_output '"reason":"sibling-prs"' "JSON output identifies sibling evidence"
check_output '"sibling_prs":[11,12,13]' "exact check names avoid substring false positives"

write_prs '[
  {"number":21,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"FAILURE"}]},
  {"number":22,"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"CANCELLED"}]},
  {"number":23,"statusCheckRollup":[{"name":"build","status":"IN_PROGRESS","conclusion":""}]}
]'
run_helper acme/widgets build
check_status 1 "fewer than three red siblings stay isolated"
check_output '2 open sibling PR(s)' "pending siblings are not counted as red"

run_helper acme/widgets build --threshold 2
check_status 0 "threshold override is honored"

write_base '{"check_runs":[
  {"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-08-30T01:00:00Z"},
  {"name":"build","status":"completed","conclusion":"success","completed_at":"2026-08-30T02:00:00Z"}
]}'
write_prs '[]'
run_helper acme/widgets build --json
check_status 1 "latest rerun wins over an older base failure"
check_output '"base_conclusion":"success"' "JSON reports the selected base conclusion"

MOCK_GH_FAIL=1 run_helper acme/widgets build
check_status 2 "GitHub lookup failures are unknown, never isolated"

run_helper not-a-repo build
check_status 2 "malformed repository names are rejected"

check_file_contains "$ROOT_DIR/src/Dockerfile" \
  'COPY bin/hive-baseline-check.sh /usr/local/bin/hive-baseline-check.sh' \
  "agent image packages the classifier"
check_file_contains "$ROOT_DIR/bin/hive-deploy.sh" \
  "sudo install -m 0755 \"\$BASELINE_HELPER_SRC\" \"\$BASELINE_HELPER_DST\"" \
  "native deploy bootstraps the classifier"

echo ""
echo "$PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
