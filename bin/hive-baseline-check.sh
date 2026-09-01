#!/usr/bin/env bash
# hive-baseline-check.sh — distinguish a PR-local check failure from a shared
# repository incident by comparing the exact check on the default branch and
# across open sibling PRs.

set -u -o pipefail

usage() {
  cat <<'EOF'
Usage: hive-baseline-check.sh <owner/repo> <check-name> [--threshold N] [--json]

Exit status:
  0  shared failure: red on the default branch or on N sibling PRs
  1  isolated failure: neither shared-failure condition was met
  2  unknown: invalid input, missing dependency, or GitHub/API failure

The sibling threshold defaults to 3 and can also be set with
HIVE_BASELINE_SIBLING_THRESHOLD.
EOF
}

error() {
  echo "hive-baseline-check: $*" >&2
  exit 2
}

if [[ $# -lt 2 ]]; then
  usage >&2
  exit 2
fi

REPO="$1"
CHECK_NAME="$2"
shift 2

THRESHOLD="${HIVE_BASELINE_SIBLING_THRESHOLD:-3}"
JSON_OUTPUT=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --threshold)
      [[ $# -ge 2 ]] || error "--threshold requires a value"
      THRESHOLD="$2"
      shift 2
      ;;
    --json)
      JSON_OUTPUT=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      error "unknown argument: $1"
      ;;
  esac
done

[[ "$REPO" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || error "repository must be owner/name"
[[ -n "$CHECK_NAME" ]] || error "check name must not be empty"
[[ "$THRESHOLD" =~ ^[1-9][0-9]*$ ]] || error "threshold must be a positive integer"
command -v gh >/dev/null 2>&1 || error "gh is required"
command -v jq >/dev/null 2>&1 || error "jq is required"

if ! REPO_JSON="$(gh api "repos/$REPO")"; then
  error "could not read repository metadata for $REPO"
fi
if ! BASE_BRANCH="$(jq -er '.default_branch | select(type == "string" and length > 0)' <<<"$REPO_JSON")"; then
  error "repository metadata did not contain a default branch"
fi

# A commit may contain older attempts of the same named check after a rerun.
# Select the newest attempt so a successful rerun clears an earlier failure.
if ! BASE_RUNS_JSON="$(gh api "repos/$REPO/commits/$BASE_BRANCH/check-runs?per_page=100")"; then
  error "could not read checks for $REPO@$BASE_BRANCH"
fi
if ! BASE_CONCLUSION="$(jq -er --arg check "$CHECK_NAME" '
    [.check_runs[]? | select(.name == $check)]
    | sort_by(.completed_at // .started_at // "")
    | (last // {})
    | (.conclusion // .status // "missing")
  ' <<<"$BASE_RUNS_JSON")"; then
  error "invalid check-run response for $REPO@$BASE_BRANCH"
fi
BASE_CONCLUSION="${BASE_CONCLUSION,,}"

if ! PRS_JSON="$(gh pr list --repo "$REPO" --state open --limit 100 --json number,statusCheckRollup)"; then
  error "could not read open PR checks for $REPO"
fi
if ! SIBLING_PRS="$(jq -cer --arg check "$CHECK_NAME" '
    [
      .[]
      | select(any(.statusCheckRollup[]?;
          ((.name // .context // "") == $check) and
          (((.conclusion // .state // "") | ascii_downcase) as $state
            | (["failure", "error", "cancelled", "timed_out", "action_required", "startup_failure", "stale"]
               | index($state)) != null)))
      | .number
    ]
    | unique
    | sort
  ' <<<"$PRS_JSON")"; then
  error "invalid PR check response for $REPO"
fi
SIBLING_COUNT="$(jq -r 'length' <<<"$SIBLING_PRS")"

BASE_RED=false
case "$BASE_CONCLUSION" in
  failure|error|cancelled|timed_out|action_required|startup_failure|stale)
    BASE_RED=true
    ;;
esac

SHARED=false
REASON="isolated"
if [[ "$BASE_RED" == true ]]; then
  SHARED=true
  REASON="default-branch"
elif (( SIBLING_COUNT >= THRESHOLD )); then
  SHARED=true
  REASON="sibling-prs"
fi

if [[ "$JSON_OUTPUT" == true ]]; then
  jq -cn \
    --arg repo "$REPO" \
    --arg check "$CHECK_NAME" \
    --arg base_branch "$BASE_BRANCH" \
    --arg base_conclusion "$BASE_CONCLUSION" \
    --arg reason "$REASON" \
    --argjson shared "$SHARED" \
    --argjson threshold "$THRESHOLD" \
    --argjson sibling_prs "$SIBLING_PRS" \
    '{repo:$repo, check:$check, shared:$shared, reason:$reason,
      base_branch:$base_branch, base_conclusion:$base_conclusion,
      sibling_threshold:$threshold, sibling_prs:$sibling_prs}'
elif [[ "$REASON" == "default-branch" ]]; then
  printf 'SHARED: check "%s" on %s default branch "%s" is red (%s).\n' \
    "$CHECK_NAME" "$REPO" "$BASE_BRANCH" "$BASE_CONCLUSION"
elif [[ "$REASON" == "sibling-prs" ]]; then
  printf 'SHARED: check "%s" is red on %s open sibling PR(s) in %s (threshold %s): %s\n' \
    "$CHECK_NAME" "$SIBLING_COUNT" "$REPO" "$THRESHOLD" "$SIBLING_PRS"
else
  printf 'ISOLATED: check "%s" is %s on %s default branch "%s" and red on %s open sibling PR(s) (threshold %s).\n' \
    "$CHECK_NAME" "$BASE_CONCLUSION" "$REPO" "$BASE_BRANCH" "$SIBLING_COUNT" "$THRESHOLD"
fi

if [[ "$SHARED" == true ]]; then
  exit 0
fi
exit 1
