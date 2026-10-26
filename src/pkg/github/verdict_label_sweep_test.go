package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A review verdict label goes back on every time the PR's head is reviewed
// again. Removed with the plain RemoveLabel, the second removal dedupes
// against the first forever and the label stays (hivecommons/hive#14361:
// in-progress and approved side by side); RemoveLabelAsOf keys each removal
// to the state it was planned from, so it reaches the API again, while a
// retry in the same state still dedupes.
func TestRemoveLabelAsOfRemovesAPutBackLabel(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

	f, c := newIssueLabelFixture(t, "review-in-progress")
	if err := c.RemoveLabelAsOf(ctx, "acme/widgets", 7, "review-in-progress", at); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLabelAsOf(ctx, "acme/widgets", 7, "review-in-progress", at); err != nil {
		t.Fatal(err)
	}
	f.readd("review-in-progress")
	if err := c.RemoveLabelAsOf(ctx, "acme/widgets", 7, "review-in-progress", at.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	deletes, labels := len(f.deletes), append([]string(nil), f.labels...)
	f.mu.Unlock()
	if deletes != 2 || len(labels) != 0 {
		t.Fatalf("deletes = %d, labels = %v; want the first and the later removal, not the retry", deletes, labels)
	}

	// The plain key is what left the label on: the put-back label survives.
	plain, pc := newIssueLabelFixture(t, "review-in-progress")
	if err := pc.RemoveLabel(ctx, "acme/widgets", 7, "review-in-progress"); err != nil {
		t.Fatal(err)
	}
	plain.readd("review-in-progress")
	if err := pc.RemoveLabel(ctx, "acme/widgets", 7, "review-in-progress"); err != nil {
		t.Fatal(err)
	}
	plain.mu.Lock()
	defer plain.mu.Unlock()
	if len(plain.deletes) != 1 || len(plain.labels) != 1 {
		t.Fatalf("plain RemoveLabel: deletes = %v, labels = %v; the regression this guards no longer reproduces", plain.deletes, plain.labels)
	}
}

func TestClosedPRsWithLabel(t *testing.T) {
	updated := time.Date(2026, 10, 26, 15, 12, 0, 0, time.UTC)
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("q")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"items": []map[string]any{
				{
					"number":         2331,
					"repository_url": "https://api.github.com/repos/acme/widgets",
					"html_url":       "https://github.com/acme/widgets/pull/2331",
					"updated_at":     updated.Format(time.RFC3339),
					"pull_request":   map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/2331"},
				},
				{
					"number":         12,
					"repository_url": "https://api.github.com/repos/acme/widgets",
					"html_url":       "https://github.com/acme/widgets/issues/12",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())

	got, err := c.ClosedPRsWithLabel(context.Background(), "acme", "hive-review/in-progress", 25)
	if err != nil {
		t.Fatal(err)
	}
	if want := `org:acme is:pr is:closed label:"hive-review/in-progress"`; query != want {
		t.Fatalf("query = %q, want %q", query, want)
	}
	if len(got) != 1 || got[0].Repo != "acme/widgets" || got[0].Number != 2331 || !got[0].UpdatedAt.Equal(updated) {
		t.Fatalf("got %+v, want only the closed PR acme/widgets#2331", got)
	}
}
