#!/usr/bin/env bash
# test-release-push-retry.sh — exercises the `push_v4` step of
# .github/workflows/release.yml (#5142) by extracting its script from the
# workflow file itself and driving it with a stubbed `git`, so the push
# state machine is proven against the shipped source rather than a copy.
#
# The step distinguishes three ways a release push to v4 can fail:
#   GH006          — branch protection has not yet ingested the gate check
#                    (propagation lag): retry on a deadline, then hard-fail.
#   non-fast-forward — v4 advanced past the release SHA: defer GREEN with
#                    pushed=false, because the successor run releases the
#                    newer merge and retagging here would publish images
#                    built from a different tree.
#   anything else  — hard-fail.
#
# Usage: src/scripts/test-release-push-retry.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)
workflow="$repo_root/.github/workflows/release.yml"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/release-push.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

if ! python3 -c 'import yaml' 2>/dev/null; then
  echo "FAIL: python3 with PyYAML is required to extract the step under test" >&2
  exit 1
fi

# --- extract the step's script verbatim from the workflow ---------------------
python3 - "$workflow" > "$tmp/push_v4.sh" <<'PY'
import sys, yaml
w = yaml.safe_load(open(sys.argv[1]))
for st in w["jobs"]["release"]["steps"]:
    if st.get("id") == "push_v4":
        sys.stdout.write(st["run"])
        break
else:
    sys.exit("no step with id push_v4 in the release job")
PY

# --- stub git + sleep ---------------------------------------------------------
# The stub reads its scenario from RPR_SCENARIO and counts branch-push attempts
# in RPR_STATE. Outputs mimic real `git push` failure text so the ORDER of the
# step's grep checks is exercised, not just their presence: GH006 rejections
# arrive inside "! [remote rejected] ... (protected branch hook declined)",
# which must NOT be captured by the non-fast-forward pattern.
mkdir -p "$tmp/bin"
cat > "$tmp/bin/git" <<'STUB'
#!/usr/bin/env bash
state="$RPR_STATE"
case "$1 $2 ${3:-}" in
  "tag "*) exit 0 ;;
  "push origin HEAD:refs/heads/v4")
    n=$(( $(cat "$state/branch" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$state/branch"
    case "$RPR_SCENARIO" in
      ok) echo "To github.com:kubestellar/hive.git"; exit 0 ;;
      gh006_then_ok)
        if [ "$n" -le 2 ]; then
          echo " ! [remote rejected] HEAD -> v4 (protected branch hook declined)"
          echo "remote: error: GH006: Protected branch update failed for refs/heads/v4."
          echo "remote: error: Required status check \"gate\" is expected."
          exit 1
        fi
        echo "To github.com:kubestellar/hive.git"; exit 0 ;;
      gh006_forever)
        echo " ! [remote rejected] HEAD -> v4 (protected branch hook declined)"
        echo "remote: error: GH006: Protected branch update failed for refs/heads/v4."
        exit 1 ;;
      nonff)
        echo " ! [rejected]        HEAD -> v4 (fetch first)"
        echo "error: failed to push some refs to 'github.com:kubestellar/hive.git'"
        echo "hint: Updates were rejected because the remote contains work that you do not"
        exit 1 ;;
      unknown)
        echo "fatal: unable to access 'https://github.com/': The requested URL returned error: 500"
        exit 1 ;;
    esac ;;
  "push origin refs/tags/"*)
    n=$(( $(cat "$state/tag" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$state/tag"
    case "${RPR_TAG_SCENARIO:-ok}" in
      ok) exit 0 ;;
      flaky_twice) [ "$n" -le 2 ] && exit 1; exit 0 ;;
      dead) exit 1 ;;
    esac ;;
  *) exit 0 ;;
esac
STUB
cat > "$tmp/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$RPR_STATE/sleeps"
exit 0
STUB
chmod +x "$tmp/bin/git" "$tmp/bin/sleep"

# run_step <scenario> [tag-scenario] [gh006-window] — runs the extracted step;
# sets rc, output, ghout (the $GITHUB_OUTPUT contents), state dir in $st.
run_step() {
  st="$tmp/state.$RANDOM"
  mkdir -p "$st"
  : > "$st/out"
  RPR_SCENARIO="$1" RPR_TAG_SCENARIO="${2:-ok}" RPR_STATE="$st" \
    RELEASE_PUSH_GH006_WINDOW="${3:-120}" \
    VERSION="4.0.1" SHA="deadbeefcafe" GITHUB_OUTPUT="$st/gh_output" \
    PATH="$tmp/bin:$PATH" bash "$tmp/push_v4.sh" > "$st/out" 2>&1
  rc=$?
  output=$(cat "$st/out")
  ghout=$(cat "$st/gh_output" 2>/dev/null || true)
}

echo "case: clean push succeeds first try"
run_step ok
[ "$rc" -eq 0 ] && note_ok "exit 0" || note_fail "exit $rc, want 0: $output"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true: $ghout"
[ "$(cat "$st/tag" 2>/dev/null)" = 1 ] && note_ok "tag pushed once" || note_fail "tag not pushed exactly once"

echo "case: GH006 propagation lag twice, then success"
run_step gh006_then_ok
[ "$rc" -eq 0 ] && note_ok "exit 0 after retries" || note_fail "exit $rc, want 0: $output"
[ "$(cat "$st/branch")" = 3 ] && note_ok "3 branch-push attempts" || note_fail "$(cat "$st/branch") attempts, want 3"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true"
grep -qx '8' "$st/sleeps" && note_ok "waited between retries" || note_fail "no 8s sleep recorded between GH006 retries"
# The [remote rejected] wrapper around GH006 must not have been eaten by the
# non-fast-forward pattern — that would defer green on a protection race and
# silently skip the release.
grep -q 'pushed=false' <<<"$ghout" && note_fail "GH006 was misclassified as non-fast-forward and deferred" || note_ok "GH006 not misread as non-fast-forward"

echo "case: GH006 past the deadline hard-fails"
run_step gh006_forever ok 0
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "exhausted GH006 retries must fail, got exit 0"
grep -q '::error::' <<<"$output" && note_ok "::error:: emitted" || note_fail "no ::error:: on exhaustion: $output"
[ -f "$st/tag" ] && note_fail "tag was pushed despite the branch push failing" || note_ok "no tag pushed"

echo "case: non-fast-forward defers green with pushed=false"
run_step nonff
[ "$rc" -eq 0 ] && note_ok "exit 0 (deferral is not a failure)" || note_fail "exit $rc, want 0: $output"
grep -q '^pushed=false$' <<<"$ghout" && note_ok "pushed=false" || note_fail "GITHUB_OUTPUT lacks pushed=false: $ghout"
grep -q '::notice::' <<<"$output" && note_ok "::notice:: explains the deferral" || note_fail "deferral is silent: $output"
[ -f "$st/tag" ] && note_fail "tag was pushed for a release that deferred" || note_ok "no tag pushed"
[ "$(cat "$st/branch")" = 1 ] && note_ok "no pointless retry of a lost race" || note_fail "non-FF was retried; it can never succeed without a rebase"

echo "case: unrecognized push failure hard-fails"
run_step unknown
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "unknown failure must not be retried or deferred, got exit 0"
grep -q 'pushed=' <<<"$ghout" && note_fail "unknown failure still wrote a pushed= output" || note_ok "no pushed= output"

echo "case: transient tag-push failure is retried"
run_step ok flaky_twice
[ "$rc" -eq 0 ] && note_ok "exit 0" || note_fail "exit $rc, want 0: $output"
[ "$(cat "$st/tag")" = 3 ] && note_ok "3 tag-push attempts" || note_fail "$(cat "$st/tag") tag attempts, want 3"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true"

echo "case: tag push dead after 3 attempts hard-fails"
run_step ok dead
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "a tag that never pushed must fail the run, got exit 0"
grep -q '^pushed=true$' <<<"$ghout" && note_fail "pushed=true written although the tag never landed" || note_ok "no pushed=true"

# --- structural pins on the workflow itself -----------------------------------
# These can't be driven as scripts (they are `if:` expressions and job wiring),
# so pin them the way the Justfile tests pin recipes: by asserting the source.
echo "case: workflow wiring"
python3 - "$workflow" <<'PY' && note_ok "precheck + release-gating wiring intact" || fail=1
import sys, yaml
w = yaml.safe_load(open(sys.argv[1]))
jobs = w["jobs"]
ok = True
def bad(msg):
    global ok; ok = False; print(f"  FAIL: {msg}")
pre = jobs.get("precheck")
if not pre:
    bad("the precheck job is gone — a superseded run will re-enter the gate dance it is guaranteed to lose (#5142)")
elif "proceed" not in (pre.get("outputs") or {}):
    bad("precheck no longer exposes a proceed output")
rel = jobs.get("release", {})
needs = rel.get("needs")
needs = [needs] if isinstance(needs, str) else (needs or [])
if "precheck" not in needs:
    bad("the release job no longer waits on precheck")
if "needs.precheck.outputs.proceed == 'true'" not in (rel.get("if") or ""):
    bad("the release job no longer honors precheck's proceed output")
gh_release = next((s for s in rel.get("steps", [])
                   if "Create GitHub Release" in (s.get("name") or "")), None)
if gh_release is None:
    bad("the Create GitHub Release step was not found")
elif "steps.push_v4.outputs.pushed == 'true'" not in (gh_release.get("if") or ""):
    bad("Create GitHub Release is not gated on pushed=true — a deferred run would "
        "publish a Release for a tag that was never pushed")
sys.exit(0 if ok else 1)
PY

if [ "$fail" -ne 0 ]; then
  echo "FAILED"
  exit 1
fi
echo "PASSED"
