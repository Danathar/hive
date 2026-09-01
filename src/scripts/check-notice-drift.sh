#!/usr/bin/env bash
# check-notice-drift.sh — compare the committed NOTICE with fresh output and
# explain the repository's byte-exact commit contract when they differ.
#
# Usage: src/scripts/check-notice-drift.sh [committed-notice] [generated-notice]
set -euo pipefail

COMMITTED_NOTICE="${1:-NOTICE}"
GENERATED_NOTICE="${2:-/tmp/NOTICE.generated}"

if ! diff -u -- "${COMMITTED_NOTICE}" "${GENERATED_NOTICE}"; then
  echo "::error::NOTICE is out of date with src/go.mod / src/go.sum. Run 'src/scripts/generate-notice.sh' locally (requires a Go toolchain). The comparison is byte-for-byte and whitespace-significant; commit the regenerated NOTICE verbatim without reformatting it." >&2
  exit 1
fi

echo "NOTICE matches the current module graph."
