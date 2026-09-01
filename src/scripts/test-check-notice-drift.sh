#!/usr/bin/env bash
# Regression coverage for the notice-drift failure contract (#5064).
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${SCRIPT_DIR}/check-notice-drift.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

COMMITTED="${TMP}/NOTICE.committed"
GENERATED="${TMP}/NOTICE.generated"

printf 'license text\n' > "${COMMITTED}"
cp "${COMMITTED}" "${GENERATED}"
output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"
grep -qF 'NOTICE matches the current module graph.' <<< "${output}"

# Whitespace-only drift is still real drift: NOTICE is compared byte-for-byte.
printf 'license text \n' > "${GENERATED}"
if output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"; then
  echo "whitespace-only NOTICE drift passed unexpectedly" >&2
  exit 1
fi

for required in \
  'src/scripts/generate-notice.sh' \
  'byte-for-byte' \
  'whitespace-significant' \
  'commit the regenerated NOTICE verbatim'; do
  if ! grep -qF -- "${required}" <<< "${output}"; then
    echo "notice-drift diagnostic omitted '${required}'" >&2
    echo "output: ${output}" >&2
    exit 1
  fi
done

echo "notice-drift checker tests: PASS"
