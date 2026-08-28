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

// Regression tests for kubestellar/hive#4928: CreatePR hardcoded base="main"
// when the caller did not pin one, so every PR the hive opened on a repository
// whose default branch is not "main" was based on the wrong branch. The diff
// then carried the whole divergence between the two branches (one reported PR
// showed 233 files / ~260k lines for a one-file change), and merging it landed
// the change on a branch nobody meant to touch.

// prBaseCapture is a fake GitHub that answers the three calls CreatePR can make
// — repo metadata, the dedupe lookup, and the create — and records the base the
// create actually asked for.
type prBaseCapture struct {
	defaultBranch string
	repoStatus    int // non-zero to fail the metadata lookup
	repoCalls     int32
	gotBase       string
}

func (p *prBaseCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/org/repo"):
			atomic.AddInt32(&p.repoCalls, 1)
			if p.repoStatus != 0 {
				w.WriteHeader(p.repoStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": p.defaultBranch})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{}) // no existing PR for this head
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.gotBase = body["base"]
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "html_url": "https://github.com/org/repo/pull/7",
				"user": map[string]any{"login": "hive[bot]"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreatePR_EmptyBaseTargetsRepoDefaultBranch(t *testing.T) {
	fake := &prBaseCapture{defaultBranch: "testing"}
	c := NewClientForTest(fake.server(t).URL, "org", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "org/repo", "feature", "", "title", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if res.Number != 7 {
		t.Fatalf("Number = %d, want 7", res.Number)
	}
	if fake.gotBase != "testing" {
		t.Fatalf("PR opened with base %q, want %q — the repository's default branch, not an assumed 'main'", fake.gotBase, "testing")
	}
}

func TestCreatePR_WhitespaceBaseTargetsRepoDefaultBranch(t *testing.T) {
	fake := &prBaseCapture{defaultBranch: "testing"}
	c := NewClientForTest(fake.server(t).URL, "org", nil, prTestLogger())

	if _, err := c.CreatePR(context.Background(), "org/repo", "feature", "   ", "title", "body"); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if fake.gotBase != "testing" {
		t.Fatalf("PR opened with base %q, want %q", fake.gotBase, "testing")
	}
}

func TestCreatePR_ExplicitBaseIsNotOverridden(t *testing.T) {
	// A caller that deliberately targets a release line (or a stacked PR's
	// parent) must keep that base, and must not pay for a metadata lookup.
	fake := &prBaseCapture{defaultBranch: "testing"}
	c := NewClientForTest(fake.server(t).URL, "org", nil, prTestLogger())

	if _, err := c.CreatePR(context.Background(), "org/repo", "feature", "release-1.2", "title", "body"); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if fake.gotBase != "release-1.2" {
		t.Fatalf("PR opened with base %q, want the explicitly requested release-1.2", fake.gotBase)
	}
	if n := atomic.LoadInt32(&fake.repoCalls); n != 0 {
		t.Fatalf("explicit base still issued %d default-branch lookups, want 0", n)
	}
}

func TestCreatePR_EmptyBaseFallsBackWhenLookupFails(t *testing.T) {
	// Metadata unreachable: opening against the fallback still beats not
	// opening the PR at all, so the failure must degrade rather than error.
	fake := &prBaseCapture{repoStatus: http.StatusInternalServerError}
	c := NewClientForTest(fake.server(t).URL, "org", nil, prTestLogger())

	if _, err := c.CreatePR(context.Background(), "org/repo", "feature", "", "title", "body"); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if fake.gotBase != FallbackDefaultBranch {
		t.Fatalf("PR opened with base %q, want fallback %q", fake.gotBase, FallbackDefaultBranch)
	}
}

func TestCreatePR_DefaultBranchResolvedOncePerRepo(t *testing.T) {
	fake := &prBaseCapture{defaultBranch: "testing"}
	c := NewClientForTest(fake.server(t).URL, "org", nil, prTestLogger())

	for _, head := range []string{"feature-a", "feature-b", "feature-c"} {
		if _, err := c.CreatePR(context.Background(), "org/repo", head, "", "title", "body"); err != nil {
			t.Fatalf("CreatePR %s: %v", head, err)
		}
		if fake.gotBase != "testing" {
			t.Fatalf("head %s opened with base %q, want testing", head, fake.gotBase)
		}
	}
	if n := atomic.LoadInt32(&fake.repoCalls); n != 1 {
		t.Fatalf("default branch resolved %d times across 3 PR opens, want 1", n)
	}
}
