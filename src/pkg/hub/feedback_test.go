package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
