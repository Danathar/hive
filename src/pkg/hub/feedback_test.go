package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHubFeedbackCreateIssueRetriesWithoutLabels(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/issues" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		calls++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if calls == 1 {
			if _, ok := payload["labels"]; !ok {
				t.Fatal("first request did not include labels")
			}
			http.Error(w, "labels denied", http.StatusForbidden)
			return
		}
		if _, ok := payload["labels"]; ok {
			t.Fatal("retry still included labels")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/hivecommons/hive/issues/42", "id": 420})
	}))
	defer srv.Close()
	req := feedbackReportRequest{Title: "Bug from dashboard", Description: "Something bad happened", RequestType: feedbackTypeBug, TargetRepo: feedbackTargetHive, IncludeDiagnostics: true, Diagnostics: &feedbackDiagnostics{HiveID: "hive-one"}}
	res, warning, err := createHubFeedbackIssue(context.Background(), srv.Client(), "tok", req, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 42 || !strings.Contains(warning, "without labels") || calls != 2 {
		t.Fatalf("res=%+v warning=%q calls=%d", res, warning, calls)
	}
}

func TestHubFeedbackRateLimiter(t *testing.T) {
	var l feedbackRateLimiter
	for i := 0; i < feedbackHubMaxPerHivePerWindow; i++ {
		if _, err := l.reserve("hive-one", time.Unix(int64(i), 0)); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if _, err := l.reserve("hive-one", time.Unix(99, 0)); err != errFeedbackRateLimited {
		t.Fatalf("err=%v want rate limited", err)
	}
}

func TestHubFeedbackIssuesRequiresBearerAndReturnsState(t *testing.T) {
	var sawAuth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/issues/42" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":      "Feedback bug",
			"state":      "open",
			"html_url":   "https://github.com/hivecommons/hive/issues/42",
			"updated_at": "2026-10-02T12:00:00Z",
			"comments":   4,
		})
	}))
	defer gh.Close()
	s := npsTestHub("h1")
	s.envGitHubToken = "hub-token"
	q := url.Values{}
	q.Set("hive_id", "h1")
	q.Set("refs", "hivecommons/hive#42")
	req := httptest.NewRequest(http.MethodGet, feedbackIssuesPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+s.heartbeatKeyFor("h1"))
	rec := httptest.NewRecorder()
	oldBase := feedbackGitHubAPIBase
	feedbackGitHubAPIBase = gh.URL
	t.Cleanup(func() { feedbackGitHubAPIBase = oldBase })
	s.handleFeedbackIssues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if sawAuth != "Bearer hub-token" {
		t.Fatalf("GitHub auth = %q", sawAuth)
	}
	var out struct {
		Items []feedbackIssueStatus `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Title != "Feedback bug" || out.Items[0].Comments != 4 {
		t.Fatalf("items = %+v", out.Items)
	}

	bad := httptest.NewRecorder()
	s.handleFeedbackIssues(bad, httptest.NewRequest(http.MethodGet, feedbackIssuesPath+"?"+q.Encode(), nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", bad.Code)
	}
}
