package github

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// bin/hive-open-pr.sh is what agents run INSTEAD of `gh pr create`; it writes
// the PRRequest this package's watcher consumes. It used to default BASE to
// "main", which meant an agent that (correctly) said nothing about the base
// still pinned every PR to "main" — so no amount of default-branch resolution
// inside CreatePR could have helped: the request already carried the wrong
// answer (kubestellar/hive#4928).
//
// These tests exercise the real script, following gh_app_token_script_test.go,
// rather than a paraphrase of it.

const hiveOpenPRScriptPath = "../../../bin/hive-open-pr.sh"

// stageHiveOpenPRScript copies the real script into a temp root with its
// request dir redirected there, returning the staged script path, the request
// dir, and the root (usable as cwd and as scratch for body files).
func stageHiveOpenPRScript(t *testing.T) (scriptPath, reqDir, root string) {
	t.Helper()
	src, err := os.ReadFile(hiveOpenPRScriptPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "hive-open-pr.sh not readable from this package: %v", err)
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}

	root = t.TempDir()
	reqDir = filepath.Join(root, "pr-requests")

	// Point the hard-coded request dir at the temp root. If this literal ever
	// stops matching, the test would silently write to the real /var/run path
	// (or nowhere), so fail loudly instead.
	text := string(src)
	const reqDirLiteral = "/var/run/hive-metrics/pr-requests"
	if !strings.Contains(text, reqDirLiteral) {
		t.Fatalf("hive-open-pr.sh no longer references %s; this test would cover nothing", reqDirLiteral)
	}
	scriptPath = filepath.Join(root, "hive-open-pr.sh")
	if err := os.WriteFile(scriptPath, []byte(strings.ReplaceAll(text, reqDirLiteral, reqDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath, reqDir, root
}

// runHiveOpenPR executes the real script with its request dir redirected into a
// temp root, and returns the single request it wrote.
func runHiveOpenPR(t *testing.T, args ...string) PRRequest {
	t.Helper()
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=scanner")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hive-open-pr.sh %v: %v\n%s", args, err, out)
	}

	entries, err := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d request files, want 1: %v", len(entries), entries)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("request is not valid PRRequest JSON (%s): %v", data, err)
	}
	return req
}

// runHiveOpenPRExpectingRefusal executes the script expecting it to FAIL: it
// must exit non-zero, write NO request file, and explain itself on stderr.
// Returns the combined output for message assertions.
func runHiveOpenPRExpectingRefusal(t *testing.T, args ...string) string {
	t.Helper()
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=scanner")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hive-open-pr.sh %v succeeded, want a loud refusal\n%s", args, out)
	}
	entries, globErr := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused invocation must write no request, wrote: %v", entries)
	}
	return string(out)
}

// TestHiveOpenPRScript_OmittedBaseLeavesBaseUnset asserts the invariant this
// bug broke: an agent that says nothing about --base must not have the script
// silently pin "main" into the request. A test that only checked "the request
// has SOME base" would pass even with the old BASE="main" default.
func TestHiveOpenPRScript_OmittedBaseLeavesBaseUnset(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--title", "fix a thing", "--body", "body")

	if req.Base != "" {
		t.Fatalf("request pinned base %q; an omitted --base must stay empty so the hive "+
			"resolves the repository's default branch", req.Base)
	}
	if req.Repo != "projectbluefin/dakota" || req.Head != "hive/fix-1" || req.Title != "fix a thing" {
		t.Fatalf("request lost fields: %+v", req)
	}
}

func TestHiveOpenPRScript_ExplicitBaseIsPreserved(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--base", "release-1.2", "--title", "fix a thing", "--body", "body")

	if req.Base != "release-1.2" {
		t.Fatalf("request base = %q, want the explicitly requested release-1.2", req.Base)
	}
}

func TestHiveOpenPRScript_ExplicitBaseEqualsFormIsPreserved(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo=projectbluefin/dakota", "--head=hive/fix-1",
		"--base=release-1.2", "--title=fix a thing", "--body=body")

	if req.Base != "release-1.2" {
		t.Fatalf("request base = %q, want release-1.2", req.Base)
	}
}

// The empty-body family pins the fix for the footer-only PRs (e.g.
// Danathar/atomic-image-builder#223): the agent wrote a full body to a file
// and passed `--body-file`, which the old parser silently dropped, so the
// request carried body:"" and the opened PR's only content was the
// attribution trailer. The script must (a) honor --body-file exactly as gh
// does, and (b) refuse loudly to submit an empty body at all.

func TestHiveOpenPRScript_BodyFileIsRead(t *testing.T) {
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)
	bodyPath := filepath.Join(root, "pr-body.md")
	const body = "## Change\n\nfixes the thing\n\nCloses #12\n"
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath,
		"--repo", "o/r", "--head", "quality/fix-12",
		"--title", "[quality] fix", "--body-file", bodyPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=quality")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hive-open-pr.sh --body-file: %v\n%s", err, out)
	}

	entries, err := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("wrote %d request files (err %v), want 1", len(entries), err)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	// $(cat) drops trailing newlines; that is fine for a PR body, so compare
	// with the same trim rather than pretending the script preserves them.
	if req.Body != strings.TrimRight(body, "\n") {
		t.Fatalf("request body = %q, want the file's content %q", req.Body, body)
	}
}

func TestHiveOpenPRScript_EmptyBodyIsRefused(t *testing.T) {
	out := runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix")
	if !strings.Contains(out, "empty body") {
		t.Fatalf("refusal must say the body is empty, got:\n%s", out)
	}
}

func TestHiveOpenPRScript_WhitespaceBodyIsRefused(t *testing.T) {
	runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body", " \n\t ")
}

func TestHiveOpenPRScript_MissingBodyFileIsRefused(t *testing.T) {
	out := runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body-file", "/nonexistent/pr-body.md")
	if !strings.Contains(out, "not readable") && !strings.Contains(out, "does not exist") {
		t.Fatalf("refusal must name the unreadable file, got:\n%s", out)
	}
}

// --issues declares the originating issue(s); the watcher verifies the body
// references each one. "#" prefixes, repetition, and comma lists all normalize.
func TestHiveOpenPRScript_IssuesFlagLandsInRequest(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "o/r", "--head", "quality/fix-12",
		"--title", "[quality] fix", "--body", "Closes #12, Closes #34, Refs #56 — docs half stays open",
		"--issues", "12,#34", "--issue", "56")
	if len(req.IssueN) != 3 || req.IssueN[0] != 12 || req.IssueN[1] != 34 || req.IssueN[2] != 56 {
		t.Fatalf("request issues = %v, want [12 34 56]", req.IssueN)
	}
}

func TestHiveOpenPRScript_NonNumericIssueIsRefused(t *testing.T) {
	runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body", "Closes #12", "--issues", "twelve")
}
