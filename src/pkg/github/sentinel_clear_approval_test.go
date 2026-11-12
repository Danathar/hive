package github

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/sentinel"
)

// sentinelClearTimelineLabel is sentinelTimelineLabel with the event id
// GitHub stamps, which is what a recorded approval is keyed by.
func sentinelClearTimelineLabel(kind, actor, actorType, at string, id int64) map[string]any {
	ev := sentinelTimelineLabel(kind, actor, actorType, at)
	ev["id"] = id
	return ev
}

// sentinelClearRecorder stands in for SentinelSweepOptions.ClearApproval: it
// keeps every clearance the sweep hands over and answers as told.
type sentinelClearRecorder struct {
	mu     sync.Mutex
	seen   []SentinelClearance
	answer SentinelClearApproval
}

func (r *sentinelClearRecorder) decide(cl SentinelClearance) SentinelClearApproval {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, cl)
	return r.answer
}

func (r *sentinelClearRecorder) calls() []SentinelClearance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SentinelClearance(nil), r.seen...)
}

func sentinelClearOpts(rec *sentinelClearRecorder) SentinelSweepOptions {
	opts := defaultSentinelOpts(nil)
	if rec != nil {
		opts.ClearApproval = rec.decide
	}
	return opts
}

const sentinelClearAlertHead = "h9"

// sentinelClearFixture is a PR by eve at head h9 that the hive alerted on
// and labelled: the hive's own alert comment is on it and the label is
// still there. unlabel takes the label off as GitHub would show it.
func sentinelClearFixture(t *testing.T) (*sentinelFixture, string, func(actor, actorType, at string)) {
	t.Helper()
	alert := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), sentinelClearAlertHead, "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(9, "eve", sentinelClearAlertHead, "sentinel-alert")},
		files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists:  true,
		comments:     map[int][]string{9: {alert}},
		commentUsers: map[int][]string{9: {"hive[bot]"}},
		checkSuites:  map[string][]string{sentinelClearAlertHead: {"2026-11-12T16:00:00Z"}},
		timeline: map[int][]map[string]any{9: {
			sentinelClearTimelineLabel("labeled", "hive[bot]", "Bot", "2026-11-12T16:05:00Z", 7001),
		}},
	}
	unlabel := func(actor, actorType, at string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prs[0] = sentinelPR(9, "eve", sentinelClearAlertHead)
		f.timeline[9] = append(f.timeline[9], sentinelClearTimelineLabel("unlabeled", actor, actorType, at, 7002))
	}
	return f, alert, unlabel
}

func sentinelClearClient(t *testing.T, f *sentinelFixture) (*Client, func()) {
	t.Helper()
	srv := newSentinelServer(t, f)
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin("hive[bot]")
	return c, srv.Close
}

func sentinelClearSweep(t *testing.T, c *Client, opts SentinelSweepOptions) *SentinelSweepResult {
	t.Helper()
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return res
}

// Off (the default): a trusted merger's removal clears Sentinel exactly as
// before. Nothing is remembered about the labelled head, the alert comment
// is left alone and no approval hook exists to call.
func TestSentinelClearApprovalOffLeavesClearanceAsItWas(t *testing.T) {
	f, alert, unlabel := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	sentinelClearSweep(t, c, sentinelClearOpts(nil))
	if c.sentinelAwaitingClear.isSeen(sentinelKey("o", "r", 9), sentinelClearAlertHead) {
		t.Fatal("the labelled head was remembered with the setting off")
	}
	unlabel("maintainer", "User", "2026-11-12T17:12:00Z")
	for pass := 0; pass < 2; pass++ {
		res := sentinelClearSweep(t, c, sentinelClearOpts(nil))
		if len(res.Flagged) != 0 || len(res.Errors) != 0 || len(f.labels[9]) != 0 {
			t.Fatalf("pass %d: cleared alert must stay quiet: res=%+v labels=%v", pass, res, f.labels[9])
		}
		if len(f.comments[9]) != 1 || f.comments[9][0] != alert {
			t.Fatalf("pass %d: alert comment changed with the setting off: %v", pass, f.comments[9])
		}
	}
}

// On: a person's removal of the label on the head the sweep saw labelled is
// handed to ClearApproval once, with the actor, id and time of GitHub's
// label event; when that approves, the hive's own alert comment gains the
// note and no second comment appears. Later sweeps hand nothing over again.
func TestSentinelClearApprovalTrustedRemovalOnAlertedHead(t *testing.T) {
	f, alert, unlabel := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Approved: true, Recorded: true}}
	opts := sentinelClearOpts(rec)

	sentinelClearSweep(t, c, opts)
	if len(rec.calls()) != 0 {
		t.Fatalf("approval asked while the label is still on: %+v", rec.calls())
	}
	unlabel("maintainer", "User", "2026-11-12T17:12:00Z")
	for pass := 0; pass < 3; pass++ {
		res := sentinelClearSweep(t, c, opts)
		if len(res.Flagged) != 0 || len(res.Errors) != 0 || len(f.labels[9]) != 0 {
			t.Fatalf("pass %d: cleared alert must stay quiet: res=%+v labels=%v", pass, res, f.labels[9])
		}
		calls := rec.calls()
		if len(calls) != 1 {
			t.Fatalf("pass %d: ClearApproval called %d times, want once", pass, len(calls))
		}
		got := calls[0]
		wantAt := time.Date(2026, 11, 12, 17, 12, 0, 0, time.UTC)
		if got.Repo != "o/r" || got.Number != 9 || got.HeadSHA != sentinelClearAlertHead || got.Label != "sentinel-alert" ||
			got.Actor != "maintainer" || got.EventID != 7002 || !got.At.Equal(wantAt) || got.PRAuthor != "eve" || got.CodeAuthor != "eve" {
			t.Fatalf("clearance = %+v", got)
		}
		if len(f.comments[9]) != 1 {
			t.Fatalf("pass %d: the note must go on the hive's own comment, not a new one: %d comments", pass, len(f.comments[9]))
		}
		body := f.comments[9][0]
		if !strings.HasPrefix(body, strings.TrimRight(alert, "\n")) || !strings.Contains(body, sentinelAlertHeading) {
			t.Fatalf("pass %d: the alert for this head must stay readable: %q", pass, body)
		}
		if !strings.Contains(body, "@maintainer removed the `sentinel-alert` label on head `h9`") || !strings.Contains(body, "sentinel.clear_counts_as_approval") {
			t.Fatalf("pass %d: comment lacks the approval note: %q", pass, body)
		}
		if n := strings.Count(body, "counts as their approval"); n != 1 {
			t.Fatalf("pass %d: approval note written %d times", pass, n)
		}
	}
}

// A restart keeps the labelled head (the map is persisted next to the
// evaluated heads), so a label removed while the hive was down is still
// picked up, and picked up once.
func TestSentinelClearApprovalSurvivesRestart(t *testing.T) {
	f, _, unlabel := sentinelClearFixture(t)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	heads := filepath.Join(t.TempDir(), "sentinel-heads.json")
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Approved: true, Recorded: true}}
	client := func() *Client {
		c := newTestClient(t, srv, "o", []string{"o/r"})
		c.SetAppBotLogin("hive[bot]")
		c.SetSentinelHeadsPath(heads)
		return c
	}
	sentinelClearSweep(t, client(), sentinelClearOpts(rec))
	unlabel("maintainer", "User", "2026-11-12T17:12:00Z")
	for restart := 0; restart < 3; restart++ {
		res := sentinelClearSweep(t, client(), sentinelClearOpts(rec))
		if len(res.Errors) != 0 || len(f.labels[9]) != 0 {
			t.Fatalf("restart %d: res=%+v labels=%v", restart, res, f.labels[9])
		}
		if n := len(rec.calls()); n != 1 {
			t.Fatalf("restart %d: ClearApproval called %d times, want once", restart, n)
		}
	}
	if n := strings.Count(f.comments[9][0], "counts as their approval"); len(f.comments[9]) != 1 || n != 1 {
		t.Fatalf("after restarts: %d comments, note written %d times", len(f.comments[9]), n)
	}
}

// A removal ClearApproval does not accept (below merger, the PR's author)
// clears Sentinel as before: no label comes back, the comment is untouched,
// and the removal is not asked about again.
func TestSentinelClearApprovalRefusedRemovalClearsOnly(t *testing.T) {
	f, alert, unlabel := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Reason: "the remover is not a trusted merger"}}
	opts := sentinelClearOpts(rec)
	sentinelClearSweep(t, c, opts)
	unlabel("mallory", "User", "2026-11-12T17:12:00Z")
	for pass := 0; pass < 2; pass++ {
		res := sentinelClearSweep(t, c, opts)
		if len(res.Flagged) != 0 || len(res.Errors) != 0 || len(f.labels[9]) != 0 {
			t.Fatalf("pass %d: refused removal must still clear Sentinel: res=%+v labels=%v", pass, res, f.labels[9])
		}
		if n := len(rec.calls()); n != 1 {
			t.Fatalf("pass %d: ClearApproval called %d times, want once", pass, n)
		}
		if len(f.comments[9]) != 1 || f.comments[9][0] != alert {
			t.Fatalf("pass %d: comment changed though nothing was approved: %v", pass, f.comments[9])
		}
	}
}

// Removals that are not a person's act on this head never reach
// ClearApproval: another bot, one of the hive's own accounts (a dashboard
// Approve takes the label off as the hive and records its own approval), a
// removal older than the head's arrival, and a hive that cannot name its
// own accounts and so cannot vouch for its alert comment.
func TestSentinelClearApprovalNotAskedForNonHumanOrStaleRemovals(t *testing.T) {
	for _, tc := range []struct {
		name, actor, actorType, at string
		// headArrived, when set, replaces the time GitHub first saw the head.
		headArrived   string
		anonymousHive bool
	}{
		{name: "another bot", actor: "renovate[bot]", actorType: "Bot", at: "2026-11-12T17:12:00Z"},
		{name: "bot-typed account without the suffix", actor: "helper", actorType: "Bot", at: "2026-11-12T17:12:00Z"},
		{name: "the hive's own account", actor: "hive[bot]", actorType: "Bot", at: "2026-11-12T17:12:00Z"},
		{name: "removal before the head arrived", actor: "maintainer", actorType: "User", at: "2026-11-12T17:12:00Z", headArrived: "2026-11-12T18:00:00Z"},
		{name: "hive cannot name its own accounts", actor: "maintainer", actorType: "User", at: "2026-11-12T17:12:00Z", anonymousHive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, alert, unlabel := sentinelClearFixture(t)
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			if !tc.anonymousHive {
				c.SetAppBotLogin("hive[bot]")
			}
			rec := &sentinelClearRecorder{answer: SentinelClearApproval{Approved: true, Recorded: true}}
			opts := sentinelClearOpts(rec)
			sentinelClearSweep(t, c, opts)
			if tc.headArrived != "" {
				f.mu.Lock()
				f.checkSuites[sentinelClearAlertHead] = []string{tc.headArrived}
				f.mu.Unlock()
			}
			unlabel(tc.actor, tc.actorType, tc.at)
			for pass := 0; pass < 2; pass++ {
				sentinelClearSweep(t, c, opts)
			}
			if calls := rec.calls(); len(calls) != 0 {
				t.Fatalf("ClearApproval asked about %+v", calls)
			}
			for _, body := range f.comments[9] {
				if strings.Contains(body, "counts as their approval") {
					t.Fatalf("approval note written: %q", body)
				}
			}
			if !tc.anonymousHive && (len(f.comments[9]) != 1 || f.comments[9][0] != alert) {
				t.Fatalf("alert comment changed: %v", f.comments[9])
			}
		})
	}
}

// The approval is for the head the label was seen on. When the PR moves to
// a new head before the label comes off and nothing shows the removal was
// made with that head in place, nobody is asked: a new head needs a new
// decision.
func TestSentinelClearApprovalNotAskedOnADifferentHead(t *testing.T) {
	f, _, _ := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Approved: true, Recorded: true}}
	opts := sentinelClearOpts(rec)
	sentinelClearSweep(t, c, opts)

	// The label comes off and the head moves before the next sweep.
	f.mu.Lock()
	f.prs[0] = sentinelPR(9, "eve", "h10")
	f.timeline[9] = append(f.timeline[9], sentinelClearTimelineLabel("unlabeled", "maintainer", "User", "2026-11-12T17:12:00Z", 7002))
	f.checkSuites["h10"] = []string{"2026-11-12T17:30:00Z"}
	f.mu.Unlock()
	sentinelClearSweep(t, c, opts)
	if calls := rec.calls(); len(calls) != 0 {
		t.Fatalf("a removal made on h9 was offered as an approval of h10: %+v", calls)
	}
	if c.sentinelAwaitingClear.isSeen(sentinelKey("o", "r", 9), sentinelClearAlertHead) {
		t.Fatal("the old head is still awaited after the PR moved on")
	}
	for _, body := range f.comments[9] {
		if strings.Contains(body, "counts as their approval") {
			t.Fatalf("approval note written for a head nobody cleared: %q", body)
		}
	}
}

// A ledger write that fails is retried: the sweep reports the error, keeps
// the head awaited and asks again on the next pass.
func TestSentinelClearApprovalRetriesAfterAFailedRecord(t *testing.T) {
	f, _, unlabel := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Err: errors.New("disk full")}}
	opts := sentinelClearOpts(rec)
	sentinelClearSweep(t, c, opts)
	unlabel("maintainer", "User", "2026-11-12T17:12:00Z")
	res := sentinelClearSweep(t, c, opts)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "disk full") {
		t.Fatalf("failed record not reported: %+v", res.Errors)
	}
	rec.mu.Lock()
	rec.answer = SentinelClearApproval{Approved: true, Recorded: true}
	rec.mu.Unlock()
	res = sentinelClearSweep(t, c, opts)
	if len(res.Errors) != 0 || len(rec.calls()) != 2 {
		t.Fatalf("retry: errors=%v calls=%d, want none and 2", res.Errors, len(rec.calls()))
	}
	if !strings.Contains(f.comments[9][0], "counts as their approval") {
		t.Fatalf("note missing after the retry succeeded: %q", f.comments[9][0])
	}
}

// Turning the setting off forgets every awaited head, so a label removed
// while it was off is not read as an approval when it is turned on again.
func TestSentinelClearApprovalOffForgetsAwaitedHeads(t *testing.T) {
	f, _, unlabel := sentinelClearFixture(t)
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{answer: SentinelClearApproval{Approved: true, Recorded: true}}
	sentinelClearSweep(t, c, sentinelClearOpts(rec))
	if !c.sentinelAwaitingClear.isSeen(sentinelKey("o", "r", 9), sentinelClearAlertHead) {
		t.Fatal("the labelled head was not remembered with the setting on")
	}
	sentinelClearSweep(t, c, sentinelClearOpts(nil))
	unlabel("maintainer", "User", "2026-11-12T17:12:00Z")
	sentinelClearSweep(t, c, sentinelClearOpts(nil))
	sentinelClearSweep(t, c, sentinelClearOpts(rec))
	if calls := rec.calls(); len(calls) != 0 {
		t.Fatalf("a removal made with the setting off became an approval: %+v", calls)
	}
}

// A head the sweep itself alerts on with the setting on is awaited from that
// pass, so a removal before the next sweep is not missed.
func TestSentinelClearApprovalAwaitsHeadItJustAlertedOn(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(9, "eve", sentinelClearAlertHead)},
		files: map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
	}
	c, done := sentinelClearClient(t, f)
	defer done()
	rec := &sentinelClearRecorder{}
	res := sentinelClearSweep(t, c, sentinelClearOpts(rec))
	if len(res.Flagged) != 1 || len(f.labels[9]) != 1 {
		t.Fatalf("fresh head must be alerted on: res=%+v labels=%v", res, f.labels[9])
	}
	if !c.sentinelAwaitingClear.isSeen(sentinelKey("o", "r", 9), sentinelClearAlertHead) {
		t.Fatal("the head just alerted on is not awaited")
	}
}

func TestSentinelAwaitingClearPath(t *testing.T) {
	if got := sentinelAwaitingClearPath("/data/sentinel-heads.json"); got != "/data/sentinel-heads-awaiting-clear.json" {
		t.Fatalf("path = %q", got)
	}
	if got := sentinelAwaitingClearPath("  "); got != "" {
		t.Fatalf("no heads path must mean memory only, got %q", got)
	}
}
