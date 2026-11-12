package automerge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func sentinelClearanceFixture(actor string) hgithub.SentinelClearance {
	return hgithub.SentinelClearance{
		Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Label: "sentinel-alert",
		Actor: actor, EventID: 7002, At: time.Date(2026, 11, 12, 17, 12, 0, 0, time.UTC),
		PRAuthor: "eve", CodeAuthor: "eve",
	}
}

func sentinelClearTestAuthz(login string) bool {
	return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
}

// The trust test a label removal must pass is the one a GitHub Approve
// review passes: a person the merger authorizer trusts, never the App, never
// a bot, and nobody when no authorizer is configured.
func TestTrustedHumanLogin(t *testing.T) {
	trustEveryone := func(string) bool { return true }
	for _, tc := range []struct {
		name  string
		authz MergerAuthorizer
		login string
		want  bool
	}{
		{"trusted merger", sentinelClearTestAuthz, "bob", true},
		{"trusted merger, case and space", sentinelClearTestAuthz, " Bob ", true},
		{"login below merger", sentinelClearTestAuthz, "mallory", false},
		{"no authorizer configured", nil, "bob", false},
		{"empty login", trustEveryone, "", false},
		{"bot suffix even when the authorizer says yes", trustEveryone, "renovate[bot]", false},
		{"the hive App even when the authorizer says yes", trustEveryone, testHiveAppBotLogin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trustedHumanLogin(tc.authz, testHiveAppBotLogin, tc.login); got != tc.want {
				t.Fatalf("trustedHumanLogin(%q) = %v, want %v", tc.login, got, tc.want)
			}
		})
	}
}

// A trusted merger's removal is recorded once as their approval of the head
// they cleared, keyed by the label event and stamped with its time.
func TestRecordSentinelClearApprovalRecordsTrustedMergerOnce(t *testing.T) {
	store := review.NewHumanChangeRequests("")
	cl := sentinelClearanceFixture("bob")
	out := RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, cl)
	if !out.Approved || !out.Recorded || out.Err != nil {
		t.Fatalf("first record = %+v, want approved and recorded", out)
	}
	got, ok := store.For("acme/widget", 7, "sha7")
	if !ok || !got.IsApproval() || got.Actor != "bob" || got.Source != review.HumanChangeRequestSourceSentinelClear || got.SourceID != 7002 || !got.RecordedAt.Equal(cl.At) {
		t.Fatalf("stored record = %+v (found %v)", got, ok)
	}
	if strings.HasPrefix(got.Body, "approve (carried") {
		t.Fatalf("body %q would read as a carried approval to the hold guard", got.Body)
	}
	// A second sweep, or a restart, hands the same label event over again.
	for i := 0; i < 2; i++ {
		again := RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, cl)
		if !again.Approved || again.Recorded || again.Err != nil {
			t.Fatalf("repeat %d = %+v, want approved and not recorded again", i, again)
		}
	}
	if n := store.Len(); n != 1 {
		t.Fatalf("store holds %d records, want 1", n)
	}
}

// The record is in the durable ledger, so a restart neither loses it nor
// writes it a second time.
func TestRecordSentinelClearApprovalSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	cl := sentinelClearanceFixture("bob")
	if out := RecordSentinelClearApproval(review.NewHumanChangeRequests(path), sentinelClearTestAuthz, testHiveAppBotLogin, cl); !out.Recorded {
		t.Fatalf("first record = %+v", out)
	}
	restarted := review.NewHumanChangeRequests(path)
	if got := review.HumanApprovalFor(restarted, "acme/widget", 7, "sha7"); got == nil || got.Actor != "bob" {
		t.Fatalf("approval lost across a restart: %+v", got)
	}
	if out := RecordSentinelClearApproval(restarted, sentinelClearTestAuthz, testHiveAppBotLogin, cl); !out.Approved || out.Recorded {
		t.Fatalf("after restart = %+v, want approved and not recorded again", out)
	}
	if n := restarted.Len(); n != 1 {
		t.Fatalf("store holds %d records after restart, want 1", n)
	}
}

// Everyone who is not a trusted merger approving someone else's PR leaves
// the removal a Sentinel clearance and nothing more.
func TestRecordSentinelClearApprovalRefusals(t *testing.T) {
	trustEveryone := func(string) bool { return true }
	for _, tc := range []struct {
		name   string
		authz  MergerAuthorizer
		mutate func(*hgithub.SentinelClearance)
		reason string
	}{
		{"login below merger", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) { cl.Actor = "mallory" }, "not a trusted merger"},
		{"no authorizer configured", nil, func(*hgithub.SentinelClearance) {}, "not a trusted merger"},
		{"a bot", trustEveryone, func(cl *hgithub.SentinelClearance) { cl.Actor = "renovate[bot]" }, "not a trusted merger"},
		{"the hive App", trustEveryone, func(cl *hgithub.SentinelClearance) { cl.Actor = testHiveAppBotLogin }, "not a trusted merger"},
		{"the PR's author, though a trusted merger", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) { cl.PRAuthor = "Bob" }, "own author"},
		{"the author of adopted code, though a trusted merger", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) {
			cl.PRAuthor, cl.CodeAuthor = testHiveAppBotLogin, "bob"
		}, "own author"},
		{"label event without an id", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) { cl.EventID = 0 }, "incomplete"},
		{"label event without a time", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) { cl.At = time.Time{} }, "incomplete"},
		{"no head", sentinelClearTestAuthz, func(cl *hgithub.SentinelClearance) { cl.HeadSHA = " " }, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := review.NewHumanChangeRequests("")
			cl := sentinelClearanceFixture("bob")
			tc.mutate(&cl)
			out := RecordSentinelClearApproval(store, tc.authz, testHiveAppBotLogin, cl)
			if out.Approved || out.Recorded || out.Err != nil || !strings.Contains(out.Reason, tc.reason) {
				t.Fatalf("outcome = %+v, want a refusal naming %q", out, tc.reason)
			}
			if n := store.Len(); n != 0 {
				t.Fatalf("a refused removal recorded %d decisions", n)
			}
		})
	}
	if out := RecordSentinelClearApproval(nil, sentinelClearTestAuthz, testHiveAppBotLogin, sentinelClearanceFixture("bob")); out.Approved || out.Reason == "" {
		t.Fatalf("no ledger = %+v, want a refusal", out)
	}
}

// The recorded approval answers a requires_human verdict on the head that
// was cleared, whether the verdict came before or after it, and answers
// nothing on a new head.
func TestSentinelClearApprovalAnswersRequiresHumanOnThatHeadOnly(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	store := review.NewHumanChangeRequests("")
	c.humanChangeRequests = store
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	ctx := context.Background()

	if login, ok := c.trustedHumanApprovalForRequiresHuman(ctx, "acme", "widget", 7, "sha7", asked); ok {
		t.Fatalf("requires_human answered by %q before anyone cleared the label", login)
	}
	// A removal by someone below merger records nothing, so the verdict
	// still parks.
	RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, sentinelClearanceFixture("mallory"))
	if login, ok := c.trustedHumanApprovalForRequiresHuman(ctx, "acme", "widget", 7, "sha7", asked); ok {
		t.Fatalf("requires_human answered by %q after an untrusted removal", login)
	}
	if out := RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, sentinelClearanceFixture("bob")); !out.Recorded {
		t.Fatalf("trusted removal = %+v", out)
	}
	// Twice: the verdict that was waiting, and one a later reviewer run
	// returns on the same head.
	for i := 0; i < 2; i++ {
		if login, ok := c.trustedHumanApprovalForRequiresHuman(ctx, "acme", "widget", 7, "sha7", asked); !ok || login != "bob" {
			t.Fatalf("read %d: requires_human on the cleared head not answered: %q %v", i, login, ok)
		}
	}
	if answered := c.withRecordedDecision("acme/widget", 7, "sha7", asked); !answered.DecisionAnswered {
		t.Fatalf("stage verdict on the cleared head not marked answered: %+v", answered)
	}
	if req, ok := review.DecisionAnswerFor(store, "acme/widget", 7, "sha8"); ok {
		t.Fatalf("approval of sha7 answers a new head: %+v", req)
	}
	if answered := c.withRecordedDecision("acme/widget", 7, "sha8", asked); answered.DecisionAnswered {
		t.Fatalf("stage verdict on a new head marked answered: %+v", answered)
	}
}

// A change request the same person left after removing the label is the
// newer decision: replaying the removal must not bury it.
func TestSentinelClearApprovalDoesNotBuryALaterChangeRequest(t *testing.T) {
	store := review.NewHumanChangeRequests("")
	cl := sentinelClearanceFixture("bob")
	if out := RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, cl); !out.Recorded {
		t.Fatalf("record = %+v", out)
	}
	later := review.HumanChangeRequest{
		Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 99,
		Actor: "bob", Body: "rename the flag first", RecordedAt: cl.At.Add(time.Hour),
	}
	if err := store.Record(later); err != nil {
		t.Fatal(err)
	}
	if out := RecordSentinelClearApproval(store, sentinelClearTestAuthz, testHiveAppBotLogin, cl); !out.Approved || out.Recorded {
		t.Fatalf("replay = %+v, want the existing record recognised", out)
	}
	if got := review.HumanChangeRequestFor(store, "acme/widget", 7, "sha7"); got == nil || got.Body != "rename the flag first" {
		t.Fatalf("the later change request no longer stands: %+v", got)
	}
}

// The 085 evidence carries the switch only while the sentinel alert is open.
func TestSentinelClearCountsAsApprovalReadsStagePolicy(t *testing.T) {
	on := New(nil, Options{StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{SentinelClearApproves: true} }})
	if !on.sentinelClearCountsAsApproval() {
		t.Fatal("switch on in the stage policy but reported off")
	}
	if New(nil, Options{}).sentinelClearCountsAsApproval() {
		t.Fatal("switch reported on with no stage policy wired")
	}
}
