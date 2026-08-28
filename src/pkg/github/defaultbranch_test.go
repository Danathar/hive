package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// repoMetadataServer serves GET /repos/{owner}/{repo} with the given
// default_branch and counts how many times it was asked.
func repoMetadataServer(t *testing.T, defaultBranch string, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/repos/org/repo") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": defaultBranch})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDefaultBranch_ResolvesFromRepoMetadata(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "testing", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	if got := c.DefaultBranch(context.Background(), "org", "repo"); got != "testing" {
		t.Fatalf("DefaultBranch = %q, want %q — the repo's own default branch, not an assumption", got, "testing")
	}
}

func TestDefaultBranch_CachesResolvedBranch(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "develop", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	for i := 0; i < 3; i++ {
		if got := c.DefaultBranch(context.Background(), "org", "repo"); got != "develop" {
			t.Fatalf("lookup %d: DefaultBranch = %q, want develop", i, got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("repo metadata fetched %d times, want 1 — the resolved branch must be cached", n)
	}
}

func TestDefaultBranch_FallsBackWhenLookupFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	if got := c.DefaultBranch(context.Background(), "org", "repo"); got != FallbackDefaultBranch {
		t.Fatalf("DefaultBranch on API error = %q, want %q", got, FallbackDefaultBranch)
	}
	// A failed lookup must NOT be cached: a transient 5xx would otherwise pin
	// this repo to the fallback base for the rest of the process.
	_ = c.DefaultBranch(context.Background(), "org", "repo")
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("repo metadata fetched %d times, want 2 — a failed lookup must not be cached", n)
	}
}

func TestDefaultBranch_EmptyDefaultBranchFallsBack(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	if got := c.DefaultBranch(context.Background(), "org", "repo"); got != FallbackDefaultBranch {
		t.Fatalf("DefaultBranch with empty metadata = %q, want %q", got, FallbackDefaultBranch)
	}
}

func TestDefaultBranch_NilAndEmptyInputs(t *testing.T) {
	var nilClient *Client
	if got := nilClient.DefaultBranch(context.Background(), "org", "repo"); got != FallbackDefaultBranch {
		t.Fatalf("nil receiver: got %q, want %q", got, FallbackDefaultBranch)
	}
	if got := (&Client{}).DefaultBranch(context.Background(), "org", "repo"); got != FallbackDefaultBranch {
		t.Fatalf("nil inner client: got %q, want %q", got, FallbackDefaultBranch)
	}

	var calls int32
	srv := repoMetadataServer(t, "testing", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())
	if got := c.DefaultBranch(context.Background(), "  ", "repo"); got != FallbackDefaultBranch {
		t.Fatalf("blank owner: got %q, want %q", got, FallbackDefaultBranch)
	}
	if got := c.DefaultBranch(context.Background(), "org", ""); got != FallbackDefaultBranch {
		t.Fatalf("blank repo: got %q, want %q", got, FallbackDefaultBranch)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("blank owner/repo issued %d API calls, want 0", n)
	}
}
