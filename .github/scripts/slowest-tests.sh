#!/usr/bin/env bash
# Summarize the slowest top-level test functions in a `go test -v` log.
#
# Usage: slowest-tests.sh <shard-label> <go-test-log> [count]
#
# Every v2-tests.yml shard runs `go test -v` (with the `=== RUN/PAUSE/CONT`
# noise stripped at tee time) so the log carries one
#     --- PASS: TestName (1.23s)
# line per top-level test function. This script ranks those lines and writes
# the top <count> (default 25) to the job's step summary, so the answer to
# "what is the PR gate actually waiting on?" is one click away instead of a
# local reproduction. Subtests are indented and deliberately ignored: their
# time is already included in the parent's.
#
# Never fails the job. A log with no per-test lines (compile failure, empty
# slice) just reports that and exits 0 — the test step itself is the verdict.
set -u

label=${1:?shard label}
log=${2:?go test -v log}
count=${3:-25}

if [ ! -s "$log" ]; then
  echo "slowest-tests: $log is missing or empty; nothing to rank"
  exit 0
fi

# Column-0 "--- PASS|FAIL|SKIP: Name (secs)" only. `sort -rn` on the seconds
# column; ties keep input order.
ranked=$(grep -E '^--- (PASS|FAIL|SKIP): [^ ]+ \([0-9.]+s\)$' "$log" \
  | sed -E 's/^--- (PASS|FAIL|SKIP): ([^ ]+) \(([0-9.]+)s\)$/\3\t\2\t\1/' \
  | sort -rn -k1,1 \
  | head -n "$count")

if [ -z "$ranked" ]; then
  echo "slowest-tests: no per-test timings in $log (was it run with -v?)"
  exit 0
fi

total=$(grep -cE '^--- (PASS|FAIL|SKIP): ' "$log")
{
  echo "### Slowest tests — $label ($total top-level functions)"
  echo
  echo "| seconds | test | result |"
  echo "|--------:|------|--------|"
  printf '%s\n' "$ranked" | awk -F'\t' '{ printf "| %s | `%s` | %s |\n", $1, $2, $3 }'
  echo
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
