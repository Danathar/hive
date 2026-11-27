package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestAlreadyDoneVerdict_UnverifiedLabelRemovalRetriesAfterFailure pins that
// one transient GitHub failure does not leave a relay's unverified already-done
// label on the issue for good.
//
// A contributor relay put hive/already-done on o/r#7 with the task credential,
// the verdict did not verify, and markAlreadyDoneUnverified tried to take the
// label off. That single DELETE failed (here a 502, live a pacing deadline).
// removeUnverifiedAlreadyDoneLabel only logs the failure, and nothing else
// ever tries again:
//   - removeUnverifiedAlreadyDoneLabel is called only from
//     markAlreadyDoneUnverified, and RemoveHiveLabelAddedSince only from
//     removeUnverifiedAlreadyDoneLabel;
//   - the AlreadyDoneUnverified ledger record is never read outside tests;
//   - the label and the 30-day hold both withhold the issue from offers, so a
//     second completion (and with it a second settle) never comes;
//   - the offer/withheld pass is read-only and calls no GitHub write.
//
// With no later pass to drive, the test drives the one entry point that owns
// the removal, settleIssueFromVerdictWithEvidence, once. The fake API fails
// only the FIRST label DELETE and works after that, so a retry by any means
// the hub controls would take the label off. On v6 there is no retry, the
// label stays, and the final assertion fails.
func TestAlreadyDoneVerdict_UnverifiedLabelRemovalRetriesAfterFailure(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{{
			"id": 1, "event": "labeled", "created_at": dispatched.Add(time.Minute).UTC().Format(time.RFC3339),
			"actor": map[string]any{"login": "hive-app[bot]"},
			"label": map[string]any{"name": "hive/already-done"},
		}},
	}
	backend := fx.server(t)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backendURL)

	// The first label DELETE on o/r#7 gets a transient 502; every other
	// request, and every later DELETE, reaches the working fixture.
	var deleteAttempts atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/7/labels/") {
			if deleteAttempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"message":"Server Error"}`))
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	hub, s := covK2Hub(t)
	s.deps.GHClient = ghpkg.NewClientForTest(front.URL, "o", []string{"r"}, hub.logger)
	s.deps.GHClient.SetAppBotLogin("hive-app[bot]")
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	hub.issuePRClaimMarker = nil
	hub.alreadyDoneMarker = nil

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "already resolved upstream", verdictReasonKindAlreadyDone, nil, dispatched, "ct")
	if got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneUnverified)
	}
	if n := deleteAttempts.Load(); n < 1 {
		t.Fatalf("precondition: the hub never tried to remove the relay's label (DELETE attempts = %d)", n)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	for _, l := range fx.issueLabels {
		if strings.EqualFold(l, "hive/already-done") {
			t.Fatalf("unverified hive/already-done is still on o/r#7 after one transient removal failure "+
				"(DELETE attempts = %d, labels = %v): the removal is never retried, so the issue stays withheld until a person removes the label",
				deleteAttempts.Load(), fx.issueLabels)
		}
	}
}
