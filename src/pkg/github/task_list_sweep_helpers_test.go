package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestIsGitHubStatus(t *testing.T) {
	notFound := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	if !isGitHubStatus(notFound, http.StatusNotFound) {
		t.Fatal("expected 404 ErrorResponse to match 404")
	}
	if isGitHubStatus(notFound, http.StatusForbidden) {
		t.Fatal("404 ErrorResponse must not match 403")
	}
	if isGitHubStatus(&gh.ErrorResponse{}, http.StatusNotFound) {
		t.Fatal("ErrorResponse with nil Response must not match")
	}
	if isGitHubStatus(errors.New("plain"), http.StatusNotFound) {
		t.Fatal("non-GitHub error must not match")
	}
	if isGitHubStatus(nil, http.StatusNotFound) {
		t.Fatal("nil error must not match")
	}
}

func TestClientWarnInfo(t *testing.T) {
	// nil receiver and nil logger are both no-ops rather than panics.
	var nilClient *Client
	nilClient.warn("w")
	nilClient.info("i")
	(&Client{}).warn("w")
	(&Client{}).info("i")

	var buf bytes.Buffer
	c := &Client{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	c.warn("warn-msg", "k", "v1")
	c.info("info-msg", "k", "v2")
	out := buf.String()
	for _, want := range []string{"level=WARN", "warn-msg", "k=v1", "level=INFO", "info-msg", "k=v2"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q:\n%s", want, out)
		}
	}
}

// taskListSweepFailureServer serves one eligible issue and lets the test dictate
// the HTTP status of the comment POST and close PATCH, so the sweep's error
// branches (404 → "gone", other → surfaced error) are exercised.
func taskListSweepFailureServer(t *testing.T, listStatus, commentStatus, patchStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/issues", func(w http.ResponseWriter, r *http.Request) {
		if listStatus != http.StatusOK {
			w.WriteHeader(listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number": 7,
			"body":   "- [x] done" + hiveTrailer,
			"state":  "open",
			"user":   map[string]any{"login": "hive-app[bot]", "type": "Bot"},
		}})
	})
	mux.HandleFunc("/repos/hivecommons/hive/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number":     44,
			"title":      "finish task list",
			"body":       "Refs #7",
			"state":      "closed",
			"merged_at":  "2099-01-01T00:00:00Z",
			"updated_at": "2099-01-01T00:00:00Z",
		}})
	})
	mux.HandleFunc("/repos/hivecommons/hive/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		w.WriteHeader(commentStatus)
		if commentStatus == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	})
	mux.HandleFunc("/repos/hivecommons/hive/issues/7", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(patchStatus)
		if patchStatus == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		}
	})
	return httptest.NewServer(mux)
}

func TestSweepCompletedTaskListIssues_FailurePaths(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: got %v, want ErrNoGitHubClient", err)
	}

	cases := []struct {
		name                    string
		list, comment, patch    int
		wantErr                 bool
		wantClosed, wantSkipped int
		wantSeen                int
	}{
		{name: "list fails", list: http.StatusInternalServerError, wantErr: true},
		{name: "comment 404 is gone", list: 200, comment: http.StatusNotFound, patch: 200, wantSkipped: 1, wantSeen: 1},
		{name: "comment 500 is error", list: 200, comment: http.StatusInternalServerError, patch: 200, wantSkipped: 1, wantSeen: 1},
		{name: "close 404 is gone", list: 200, comment: http.StatusCreated, patch: http.StatusNotFound, wantSkipped: 1, wantSeen: 1},
		{name: "close 500 is error", list: 200, comment: http.StatusCreated, patch: http.StatusInternalServerError, wantSkipped: 1, wantSeen: 1},
		{name: "happy path", list: 200, comment: http.StatusCreated, patch: 200, wantClosed: 1, wantSeen: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := taskListSweepFailureServer(t, tc.list, tc.comment, tc.patch)
			defer srv.Close()
			c := NewClientForTest(srv.URL, "hivecommons", []string{"hivecommons/hive"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			var audited int
			res, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{
				Audit: func(TaskListSweepEvent) { audited++ },
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(res.Closed) != tc.wantClosed || res.Skipped != tc.wantSkipped || res.Seen != tc.wantSeen {
				t.Fatalf("closed=%d skipped=%d seen=%d, want %d/%d/%d", len(res.Closed), res.Skipped, res.Seen, tc.wantClosed, tc.wantSkipped, tc.wantSeen)
			}
			if audited != tc.wantClosed {
				t.Fatalf("audit called %d times, want %d", audited, tc.wantClosed)
			}
		})
	}
}

func TestIssueLabelNames(t *testing.T) {
	if got := issueLabelNames(nil); got != nil {
		t.Fatalf("nil labels: got %v", got)
	}
	got := issueLabelNames([]*gh.Label{{Name: gh.Ptr("a")}, nil, {Name: gh.Ptr("b")}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v, want [a b]", got)
	}
}
