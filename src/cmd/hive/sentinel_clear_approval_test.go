package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func sentinelClearApprovalTestConfig(on bool) *config.Config {
	cfg := &config.Config{}
	cfg.Dashboard.AuthorizedUsers = []string{"olivia:owner", "mia:merger", "walt:read-write"}
	cfg.Sentinel.ClearCountsAsApproval = on
	return cfg
}

func sentinelClearApprovalTestStore(t *testing.T) *review.HumanChangeRequests {
	t.Helper()
	store := review.NewHumanChangeRequests("")
	prev := sentinelClearApprovalStore
	sentinelClearApprovalStore = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { sentinelClearApprovalStore = prev })
	return store
}

func sentinelClearApprovalTestClearance(actor string) hgithub.SentinelClearance {
	return hgithub.SentinelClearance{
		Repo: "acme/widgets", Number: 7, HeadSHA: "sha7", Label: "sentinel-alert",
		Actor: actor, EventID: 7002, At: time.Date(2026, 11, 12, 17, 12, 0, 0, time.UTC),
		PRAuthor: "eve", CodeAuthor: "eve",
	}
}

// Off by default: the sweep gets no approval hook at all, so a label removal
// can only ever clear Sentinel.
func TestSentinelSweepOptionsClearApprovalOffByDefault(t *testing.T) {
	ghClient := hgithub.NewClient("", "acme", []string{"acme/widgets"}, nil, "")
	if opts := sentinelSweepOptions(&config.Config{}, ghClient, nil); opts.ClearApproval != nil {
		t.Fatal("ClearApproval wired with sentinel.clear_counts_as_approval unset")
	}
	if opts := sentinelSweepOptions(sentinelClearApprovalTestConfig(false), ghClient, nil); opts.ClearApproval != nil {
		t.Fatal("ClearApproval wired with sentinel.clear_counts_as_approval false")
	}
	if opts := sentinelSweepOptions(sentinelClearApprovalTestConfig(true), ghClient, nil); opts.ClearApproval == nil {
		t.Fatal("ClearApproval not wired with sentinel.clear_counts_as_approval true")
	}
	if stageLabelPolicyFunc(sentinelClearApprovalTestConfig(false))().SentinelClearApproves || !stageLabelPolicyFunc(sentinelClearApprovalTestConfig(true))().SentinelClearApproves {
		t.Fatal("the stage policy does not follow sentinel.clear_counts_as_approval")
	}
}

// On: only a merger or owner from dashboard.authorized_users gets an
// approval recorded, the recorded decision is the approval kind on that
// head, and exactly one audit entry is written however often the same label
// event is handed over.
func TestSentinelClearApprovalFuncRecordsTrustedMergersOnly(t *testing.T) {
	ghClient := hgithub.NewClient("", "acme", []string{"acme/widgets"}, nil, "")
	ghClient.SetAppBotLogin("hive[bot]")
	for _, tc := range []struct {
		name, actor string
		mutate      func(*hgithub.SentinelClearance)
		want        bool
	}{
		{name: "merger", actor: "mia", want: true},
		{name: "owner", actor: "olivia", want: true},
		{name: "read-write is below merger", actor: "walt"},
		{name: "not in authorized_users", actor: "mallory"},
		{name: "the hive App", actor: "hive[bot]"},
		{name: "a merger on their own PR", actor: "mia", mutate: func(cl *hgithub.SentinelClearance) { cl.PRAuthor = "mia" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sentinelClearApprovalTestStore(t)
			dashSrv := dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			decide := sentinelClearApprovalFunc(sentinelClearApprovalTestConfig(true), ghClient, dashSrv)
			if decide == nil {
				t.Fatal("no hook with the setting on")
			}
			cl := sentinelClearApprovalTestClearance(tc.actor)
			if tc.mutate != nil {
				tc.mutate(&cl)
			}
			for pass := 0; pass < 2; pass++ {
				out := decide(cl)
				if out.Approved != tc.want || out.Recorded != (tc.want && pass == 0) || out.Err != nil {
					t.Fatalf("pass %d: outcome = %+v, want approved=%v", pass, out, tc.want)
				}
			}
			approval := review.HumanApprovalFor(store, "acme/widgets", 7, "sha7")
			if got := approval != nil; got != tc.want {
				t.Fatalf("approval recorded = %v, want %v (%+v)", got, tc.want, approval)
			}
			if tc.want && (approval.Actor != tc.actor || approval.Source != review.HumanChangeRequestSourceSentinelClear) {
				t.Fatalf("recorded approval = %+v", approval)
			}
			audited := 0
			for _, e := range dashSrv.GetAudit().Recent(0) {
				if e.Action != sentinelClearApprovalAuditAction {
					continue
				}
				audited++
				for _, want := range []string{"repo=acme/widgets", "pr=7", "head=sha7", "by=" + tc.actor, "label_event=7002"} {
					if !strings.Contains(e.Detail, want) {
						t.Errorf("audit detail %q lacks %q", e.Detail, want)
					}
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want]; audited != want {
				t.Fatalf("audit entries = %d, want %d", audited, want)
			}
		})
	}
}
