package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/testsuite#991 (2026-10-18..25): the hive pushed its own fix
// commit, signed off as the App, onto j1mc's fork PR (role/contributor). The
// stage pass sent it to 072 on hive-signoff-attribution and nudged j1mc twice
// to re-sign a commit they never made. A maintainer approving the head and
// merging with a merge commit was the real resolution.

// testsuite991Commits is #991's head without the merge-from-base commit:
// the contributor's self-signed commit and the hive's App-signed fix.
var testsuite991Commits = []stageCommitFixture{
	{message: "feat(gnome-e2e): gate extension-validation runs\n\nSigned-off-by: j1mc <j1mc@example.com>", authorLogin: "j1mc", authorName: "j1mc", authorEmail: "j1mc@example.com"},
	{message: "test(gnome-e2e): pin extension-validation gate step\n\nSigned-off-by: hive[bot] <12345+hive[bot]@users.noreply.github.com>", authorLogin: testHiveAppBotLogin, authorName: "hive[bot]", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
}

type signoffCase struct {
	commits    []stageCommitFixture
	reason     string
	squashOnly bool
	// approvedHead, when set, is the head a trusted maintainer approved.
	approvedHead string
	// squashGate: the head's checks include `Sign-off survives the squash`.
	squashGate bool
}

func runSignoffCase(t *testing.T, tc signoffCase) (*stageAPI, stagelabels.Record, *Engine, func()) {
	t.Helper()
	const n = 991
	api := &stageAPI{labels: map[string]string{}, commitDetails: map[int][]stageCommitFixture{n: tc.commits}}
	reviewReads := 0
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/991/reviews" {
			return false
		}
		reviewReads++
		out := []map[string]any{}
		if tc.approvedHead != "" {
			out = append(out, map[string]any{"id": 1, "state": "APPROVED", "commit_id": tc.approvedHead, "user": map[string]string{"login": "maintainer"}})
		}
		_ = json.NewEncoder(w).Encode(out)
		return true
	}
	srv := newStageAPI(t, api)
	clock := &contributorLaneClock{at: time.Date(2026, 10, 24, 17, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.SetMergerAuthorizer(func(login string) bool { return login == "maintainer" })
	e.stageLabelPolicy = func() StageLabelPolicy {
		// j1mc is a registered relay author (author/clanker), as on #991.
		p := StageLabelPolicy{Enabled: true, ContributorNudge: true, DCORequired: func(string) (bool, bool) { return true, true }, RelayKnown: func(login string) bool { return login == "j1mc" }}
		if tc.squashOnly {
			p.SquashOnly = func(string) bool { return true }
		}
		return p
	}
	pr := stagePR(n, "role/contributor", "author/clanker")
	pr.User = &gh.User{Login: gh.Ptr("j1mc")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, tc.reason, false, nil)
	e.noteStageRole("acme", "widget", n, stagelabels.RoleContributor)
	if tc.squashGate {
		e.noteStageDCOGate("acme", "widget", n, pr.GetHead().GetSHA(), nil, map[string]bool{squashSignoffCheck: true})
	}
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(stageKey("acme", "widget", n))
	return api, rec, e, func() {
		// Same head, next sweep, carrying the labels the first one wrote:
		// nothing new is posted.
		before := len(api.comments)
		pr = stagePR(n, append([]string{"role/contributor", "author/clanker"}, addedAll(api)...)...)
		pr.User = &gh.User{Login: gh.Ptr("j1mc")}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, tc.reason, false, nil)
		e.noteStageRole("acme", "widget", n, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.comments) != before {
			t.Fatalf("second sweep posted again: %q", api.comments[before:])
		}
		srv.Close()
	}
}

func commentsWith(api *stageAPI, prefix string) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func assertNoContributorAsk(t *testing.T, api *stageAPI) {
	t.Helper()
	for _, c := range api.comments {
		for _, bad := range []string{ContributorNudgeMarkerPrefix, SignoffAttributionMarkerPrefix, "back in your court", "git config user.email", "--reset-author"} {
			if strings.Contains(c, bad) {
				t.Fatalf("contributor asked to fix the hive's commit (%q):\n%s", bad, c)
			}
		}
	}
}

func addedAll(api *stageAPI) []string {
	var out []string
	for _, a := range api.added {
		out = append(out, a...)
	}
	return out
}

func TestHiveCausedSignoffAttributionParksForMaintainer(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict})
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 085 on hive-signoff-attribution", rec)
	}
	added := addedAll(api)
	if !containsLabel(added, stagelabels.NeedsDecision.Label()) || containsLabel(added, stagelabels.ReturnedToContributor.Label()) || containsLabel(added, stagelabels.MergeCommitLabel) {
		t.Fatalf("labels added %v", added)
	}
	assertNoContributorAsk(t, api)
	notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
	if len(notes) != 1 {
		t.Fatalf("maintainer note: %q", api.comments)
	}
	for _, want := range []string{"head=sha", "`stage/085-needs-decision`", "1 of 2 commits", "@j1mc did not make those commits and has nothing to fix", "**Approve**", "`gh pr merge 991 --merge`, not squash", "Ask (maintainer)"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("note missing %q:\n%s", want, notes[0])
		}
	}
	again()
}

func TestHiveCausedSignoffAttributionSquashOnlyParksNamingPolicy(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, squashOnly: true})
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 085", rec)
	}
	assertNoContributorAsk(t, api)
	notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
	if len(notes) != 1 || !strings.Contains(notes[0], "auto_merge.squash_only") || !strings.Contains(notes[0], "lift the squash-only policy") || strings.Contains(notes[0], "--merge") {
		t.Fatalf("squash-only note: %q", api.comments)
	}
	if containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("merge commit elected in a squash-only repo: %v", api.added)
	}
	again()
}

func TestHiveCausedSignoffAttributionApprovedHeadElectsMergeCommit(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: "sha"})
	if rec.Stage == stagelabels.NeedsDecision || rec.Stage == stagelabels.ReturnedToContributor || rec.Reason == stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("approved head still held on attribution: %+v", rec)
	}
	if !containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("merge-method label not written: %v", api.added)
	}
	if len(mergeCommitComments(api)) != 1 || len(commentsWith(api, SignoffAttributionDecisionMarkerPrefix)) != 0 {
		t.Fatalf("comments: %q", api.comments)
	}
	assertNoContributorAsk(t, api)
	again()
}

func TestHiveCausedSignoffAttributionStaleApprovalStillParks(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: "0ld0000"})
	if rec.Stage != stagelabels.NeedsDecision || containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("approval of an older head elected a merge commit: %+v added=%v", rec, api.added)
	}
	assertNoContributorAsk(t, api)
	again()
}

// A red check on a PR made only of the contributor's own commits is still
// theirs: 072, as before.
func TestContributorFixableFailureStillReturnsToContributor(t *testing.T) {
	own := []stageCommitFixture{testsuite991Commits[0]}
	_, rec, _, again := runSignoffCase(t, signoffCase{commits: own, reason: "check-failing:unit"})
	if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != "check-failing:unit" {
		t.Fatalf("record %+v, want 072 on check-failing:unit", rec)
	}
	again()
}

// hivecommons/pluk#315's shape on a contributor PR: every commit carries the
// App's identity and the hive never pushed the head, so it is the author's
// own commit made with the wrong identity and still goes back to them.
func TestAuthorsOwnAppSignedCommitStillReturnsToAuthor(t *testing.T) {
	appOnly := []stageCommitFixture{testsuite991Commits[1]}
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: appOnly, reason: contributorLaneReasonNoVerdict})
	if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 072 on hive-signoff-attribution", rec)
	}
	if len(commentsWith(api, SignoffAttributionDecisionMarkerPrefix)) != 0 {
		t.Fatalf("maintainer note on an author-fixable head: %q", api.comments)
	}
	again()
}
