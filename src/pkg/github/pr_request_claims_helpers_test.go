package github

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// These tests pin the pure helpers behind validatePRRequestClaims: the
// artifact-file classifiers that back title claims, the closing-reference
// downgrade rewriter that keeps multi-phase trackers open, and the repo
// resolution used for cross-repo issue lookups.

func TestIsTestFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		// stem suffixes
		{"src/retry_test.go", true},
		{"lib/parser_spec.rb", true},
		// basename prefixes
		{"test_sync.py", true},
		{"build-aux/test-publish.sh", true},
		// dotted infixes and bats
		{"web/app.test.tsx", true},
		{"web/app.spec.js", true},
		{"cli/smoke.bats", true},
		// directory segments
		{"test/fixtures/data.json", true},
		{"pkg/tests/helper.go", true},
		{"spec/models/user.rb", true},
		{"specs/api.yaml", true},
		{"src/__tests__/index.js", true},
		// case-insensitive, ./-prefixed, uncleaned paths
		{"./SRC/Retry_TEST.GO", true},
		{"a/../test/x.go", true},
		// negatives: near-misses must not count as tests
		{"src/main.go", false},
		{"src/contest.go", false},
		{"latest/main.go", false},
		{"docs/attestation.md", false},
		{"src/testdata/golden.json", false},
		{"protest/march.go", false},
	}
	for _, tt := range tests {
		if got := isTestFile(tt.filename); got != tt.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestIsMigrationFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"db/migrations/001_users.sql", true},
		{"db/migration/001_users.sql", true},
		{"scripts/migrate/step.go", true},
		{"./DB/Migrations/002.sql", true},
		{"db/schema.sql", false},
		{"docs/migrating.md", false},
		{"src/migrator.go", false},
	}
	for _, tt := range tests {
		if got := isMigrationFile(tt.filename); got != tt.want {
			t.Errorf("isMigrationFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestIsWorkflowFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{".github/workflows/ci.yml", true},
		{"./.github/workflows/release.yaml", true},
		{".github/workflows/README.md", false},
		{".github/actions/setup/action.yml", false},
		{"workflows/ci.yml", false},
	}
	for _, tt := range tests {
		if got := isWorkflowFile(tt.filename); got != tt.want {
			t.Errorf("isWorkflowFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestDowngradeClosingReferences_Rewrites(t *testing.T) {
	downgrade := map[string]string{
		claimKey("o/r", 60):        "issue is a tracker",
		claimKey("other/repo", 12): "issue has unchecked task items that delegate work to other issues",
	}
	tests := []struct {
		name string
		text string
		want string
	}{
		{"empty text", "", ""},
		{
			"bare ref resolved via default repo",
			"Fixes #60 for phase one",
			"Refs #60 for phase one",
		},
		{
			"colon and casing preserved after keyword swap",
			"CLOSES: #60",
			"Refs: #60",
		},
		{
			"cross-repo ref matched case-insensitively",
			"Resolves Other/Repo#12",
			"Refs Other/Repo#12",
		},
		{
			"ref outside downgrade map untouched",
			"Fixes #61",
			"Fixes #61",
		},
		{
			"cross-repo ref to unlisted repo untouched",
			"Fixes elsewhere/repo#60",
			"Fixes elsewhere/repo#60",
		},
		{
			"non-closing mention untouched",
			"See #60 and related to #60",
			"See #60 and related to #60",
		},
		{
			"mixed refs downgraded independently",
			"Fixes #60, closes #61, resolves other/repo#12",
			"Refs #60, closes #61, Refs other/repo#12",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := downgradeClosingReferences(tt.text, "o/r", downgrade, false); got != tt.want {
				t.Errorf("downgradeClosingReferences(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// A rewritten reference must say so in the body it lands in: before #7156 the
// only record of the rewrite was a WARN line in a container log, so a merged
// "Refs #N" was indistinguishable from an agent's deliberate choice.
func TestDowngradeClosingReferences_Annotates(t *testing.T) {
	const reason = "issue is a tracker"
	downgrade := map[string]string{claimKey("o/r", 60): reason}

	t.Run("first rewritten reference carries the reason", func(t *testing.T) {
		got := downgradeClosingReferences("## Related Issue\nCloses #60\n", "o/r", downgrade, true)
		want := "## Related Issue\nRefs #60" + downgradeNote(reason) + "\n"
		if got != want {
			t.Errorf("annotated body = %q, want %q", got, want)
		}
	})

	t.Run("reason appears once per issue", func(t *testing.T) {
		got := downgradeClosingReferences("Closes #60 and fixes #60", "o/r", downgrade, true)
		if n := strings.Count(got, "closing keyword withheld"); n != 1 {
			t.Errorf("note count = %d, want 1 (body: %q)", n, got)
		}
		if !strings.HasSuffix(got, "Refs #60") {
			t.Errorf("second reference should be rewritten without a note, got %q", got)
		}
	})

	t.Run("titles are rewritten without a note", func(t *testing.T) {
		got := downgradeClosingReferences("fix: the thing (closes #60)", "o/r", downgrade, false)
		if want := "fix: the thing (Refs #60)"; got != want {
			t.Errorf("title = %q, want %q", got, want)
		}
	})

	// The reason text quotes issue references of its own; wrapping it in a code
	// span keeps GitHub from cross-referencing an unrelated issue.
	t.Run("reason is wrapped in a code span", func(t *testing.T) {
		note := downgradeNote("see kubestellar/hive#6781")
		if !strings.Contains(note, "`see kubestellar/hive#6781`") {
			t.Errorf("note does not code-span the reason: %q", note)
		}
	})
}

// The task list the policy templates mandate for an unsplittable finding must
// NOT cost the PR its closing keyword (#7156). Only a list that delegates work
// to other issues is a tracker.
func TestHasUnfinishedDelegatedWork(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"no task list", "just prose about a bug", false},
		{
			"mandated acceptance criteria for one PR",
			"## Recommendation\n\n- [ ] add the missing paren\n- [ ] tighten the test\n",
			false,
		},
		{"prose item that merely mentions an issue", "- [ ] rename the field (see #318)", false},
		{"unchecked delegating item", "- [ ] #4199 land the parser", true},
		{"unchecked cross-repo delegating item", "* [ ] owner/repo#12", true},
		{"all delegating items ticked", "- [x] #4199\n- [x] owner/repo#12\n", false},
		{"one of several delegating items outstanding", "- [x] #4199\n- [ ] #4200\n", true},
		{
			"quoted policy example inside a fence does not count",
			"The templates say:\n\n```markdown\n- [ ] #123 one box per deliverable\n```\n\nNothing outstanding.\n",
			false,
		},
		{"indented delegating item still counts", "- [x] parent\n    - [ ] #77 child\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUnfinishedDelegatedWork(tt.body); got != tt.want {
				t.Errorf("hasUnfinishedDelegatedWork(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestPRRequestRepo_Resolution(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "defaultorg", []string{"r"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		repo      string
		wantOwner string
		wantRepo  string
	}{
		{"o/r", "o", "r"},
		{"  o/r  ", "o", "r"},
		{"bare-repo", "defaultorg", "bare-repo"},
		{"  bare-repo  ", "defaultorg", "bare-repo"},
		{"", "defaultorg", ""},
	}
	for _, tt := range tests {
		owner, repo := c.prRequestRepo(tt.repo)
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("prRequestRepo(%q) = (%q, %q), want (%q, %q)",
				tt.repo, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}

	// SetOrg changes the owner used for bare repo names.
	c.SetOrg("neworg")
	if owner, _ := c.prRequestRepo("bare-repo"); owner != "neworg" {
		t.Errorf("after SetOrg, prRequestRepo owner = %q, want %q", owner, "neworg")
	}
}

func TestIncompleteIssueReason_Classification(t *testing.T) {
	strp := func(s string) *string { return &s }
	label := func(name string) *gh.Label { return &gh.Label{Name: strp(name)} }

	tests := []struct {
		name  string
		issue *gh.Issue
		want  string
	}{
		{"nil issue", nil, "issue metadata is empty"},
		{
			"tracker label",
			&gh.Issue{Title: strp("work"), Body: strp("b"), Labels: []*gh.Label{label("Tracker")}},
			"issue is labeled as a tracker or epic",
		},
		{
			"meta-tracker label",
			&gh.Issue{Title: strp("work"), Body: strp("b"), Labels: []*gh.Label{label("meta-tracker")}},
			"issue is labeled as a tracker or epic",
		},
		{
			"epic title prefix",
			&gh.Issue{Title: strp("[epic] program"), Body: strp("b")},
			"issue title marks it as a tracker or epic",
		},
		{
			"tracker title prefix after whitespace",
			&gh.Issue{Title: strp("  [Tracker] program"), Body: strp("b")},
			"issue title marks it as a tracker or epic",
		},
		{
			"unchecked task item delegating to another issue",
			&gh.Issue{Title: strp("work"), Body: strp("- [x] #100 done\n* [ ] #101 remaining")},
			"issue has unchecked task items that delegate work to other issues",
		},
		{
			// #7156: the format every policy template mandates for an
			// unsplittable finding must not disqualify its own PR from
			// closing it.
			"mandated acceptance-criteria checklist is not incomplete",
			&gh.Issue{Title: strp("bug: missing paren"), Body: strp("## Recommendation\n\n- [ ] add the paren\n- [ ] tighten the test\n")},
			"",
		},
		{
			// #7156: the downgrade gate and the sweep must read a body the
			// same way — a quoted policy example is not a task list.
			"task list quoted inside a fenced block is not incomplete",
			&gh.Issue{Title: strp("bug"), Body: strp("policy says:\n\n```\n- [ ] #123 one box per deliverable\n```\n")},
			"",
		},
		{
			"complete issue",
			&gh.Issue{Title: strp("focused bug"), Body: strp("one acceptance criterion")},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := incompleteIssueReason(tt.issue); got != tt.want {
				t.Errorf("incompleteIssueReason = %q, want %q", got, tt.want)
			}
		})
	}
}
