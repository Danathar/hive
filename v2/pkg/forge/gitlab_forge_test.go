package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestGitLab builds a gitLabForge pointed at a test server.
func newTestGitLab(t *testing.T, serverURL, org string) *gitLabForge {
	t.Helper()
	f, err := newGitLabForge("test-token", Options{BaseURL: serverURL, Org: org})
	if err != nil {
		t.Fatalf("newGitLabForge: %v", err)
	}
	return f
}

const cankedProjectJSON = `{
	"id": 42,
	"path": "hive",
	"path_with_namespace": "kubestellar/hive",
	"web_url": "https://gitlab.com/kubestellar/hive",
	"default_branch": "main",
	"description": "Fleet automation",
	"namespace": {"full_path": "kubestellar", "path": "kubestellar"}
}`

const cannedIssuesJSON = `[
	{
		"iid": 7,
		"title": "Fix the widget",
		"state": "opened",
		"labels": ["kind/bug", "priority/high"],
		"web_url": "https://gitlab.com/kubestellar/hive/-/issues/7",
		"created_at": "2026-07-01T12:00:00Z",
		"author": {"username": "alice"},
		"assignees": [{"username": "bob"}, {"username": "carol"}]
	},
	{
		"iid": 8,
		"title": "Add docs",
		"state": "opened",
		"labels": [],
		"web_url": "https://gitlab.com/kubestellar/hive/-/issues/8",
		"created_at": "2026-07-02T09:30:00Z",
		"author": {"username": "dave"},
		"assignees": []
	}
]`

const cannedMRsJSON = `[
	{
		"iid": 101,
		"title": "Implement forge abstraction",
		"state": "opened",
		"labels": ["enhancement"],
		"web_url": "https://gitlab.com/kubestellar/hive/-/merge_requests/101",
		"created_at": "2026-07-05T08:00:00Z",
		"draft": false,
		"work_in_progress": false,
		"source_branch": "feat-forge",
		"target_branch": "main",
		"sha": "abc123def",
		"author": {"username": "erin"}
	},
	{
		"iid": 102,
		"title": "WIP: refactor",
		"state": "opened",
		"labels": [],
		"web_url": "https://gitlab.com/kubestellar/hive/-/merge_requests/102",
		"created_at": "2026-07-06T10:15:00Z",
		"draft": true,
		"work_in_progress": true,
		"source_branch": "refactor",
		"target_branch": "main",
		"sha": "def456",
		"author": {"username": "frank"}
	}
]`

func TestGitLabGetRepo(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(gitLabTokenHeader)
		// RequestURI preserves the wire-level percent-encoding; r.URL.Path is
		// already decoded by net/http and would hide the %2F.
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cankedProjectJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "kubestellar")
	repo, err := f.GetRepo(context.Background(), "hive")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}

	if gotAuth != "test-token" {
		t.Errorf("PRIVATE-TOKEN header = %q, want test-token", gotAuth)
	}
	// Bare "hive" should be namespaced with org and URL-encoded as one segment.
	if !strings.Contains(gotPath, "/api/v4/projects/") {
		t.Errorf("path = %q, want /api/v4/projects/...", gotPath)
	}
	if !strings.Contains(gotPath, "kubestellar%2Fhive") {
		t.Errorf("path = %q, want URL-encoded kubestellar%%2Fhive", gotPath)
	}

	if repo.FullName != "kubestellar/hive" {
		t.Errorf("FullName = %q", repo.FullName)
	}
	if repo.Owner != "kubestellar" {
		t.Errorf("Owner = %q", repo.Owner)
	}
	if repo.Name != "hive" {
		t.Errorf("Name = %q", repo.Name)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", repo.DefaultBranch)
	}
	if repo.URL != "https://gitlab.com/kubestellar/hive" {
		t.Errorf("URL = %q", repo.URL)
	}
	if repo.Description != "Fleet automation" {
		t.Errorf("Description = %q", repo.Description)
	}
}

func TestGitLabListOpenIssues(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		// No X-Next-Page header => single page.
		_, _ = w.Write([]byte(cannedIssuesJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "kubestellar")
	issues, err := f.ListOpenIssues(context.Background(), "kubestellar/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}

	if gotState != "opened" {
		t.Errorf("state query = %q, want opened", gotState)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}

	i0 := issues[0]
	if i0.Number != 7 || i0.Title != "Fix the widget" || i0.Author != "alice" {
		t.Errorf("issue[0] = %+v", i0)
	}
	if i0.State != "opened" {
		t.Errorf("issue[0].State = %q", i0.State)
	}
	if len(i0.Labels) != 2 || i0.Labels[0] != "kind/bug" {
		t.Errorf("issue[0].Labels = %v", i0.Labels)
	}
	if len(i0.Assignees) != 2 || i0.Assignees[0] != "bob" {
		t.Errorf("issue[0].Assignees = %v", i0.Assignees)
	}
	if i0.Repo != "kubestellar/hive" {
		t.Errorf("issue[0].Repo = %q", i0.Repo)
	}
	if i0.CreatedAt.IsZero() {
		t.Error("issue[0].CreatedAt is zero")
	}

	// Empty labels must map to a non-nil slice (guard against nil range/join).
	if issues[1].Labels == nil {
		t.Error("issue[1].Labels should be non-nil empty slice")
	}
	if len(issues[1].Assignees) != 0 {
		t.Errorf("issue[1].Assignees = %v, want empty", issues[1].Assignees)
	}
}

func TestGitLabListOpenChangeRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedMRsJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "kubestellar")
	crs, err := f.ListOpenChangeRequests(context.Background(), "kubestellar/hive")
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("got %d change requests, want 2", len(crs))
	}

	c0 := crs[0]
	if c0.Number != 101 || c0.Title != "Implement forge abstraction" || c0.Author != "erin" {
		t.Errorf("cr[0] = %+v", c0)
	}
	if c0.Draft {
		t.Error("cr[0].Draft should be false")
	}
	if c0.SourceBranch != "feat-forge" || c0.TargetBranch != "main" {
		t.Errorf("cr[0] branches = %q -> %q", c0.SourceBranch, c0.TargetBranch)
	}
	if c0.HeadSHA != "abc123def" {
		t.Errorf("cr[0].HeadSHA = %q", c0.HeadSHA)
	}

	// work_in_progress must be treated as draft.
	if !crs[1].Draft {
		t.Error("cr[1] (work_in_progress) should map to Draft=true")
	}
}

func TestGitLabPagination(t *testing.T) {
	// Two pages: first returns one issue with X-Next-Page: 2; second returns
	// one issue with no next page.
	page1 := `[{"iid":1,"title":"one","state":"opened","author":{"username":"a"}}]`
	page2 := `[{"iid":2,"title":"two","state":"opened","author":{"username":"b"}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(page1))
		case "2":
			// no X-Next-Page => last page
			_, _ = w.Write([]byte(page2))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "kubestellar")
	issues, err := f.ListOpenIssues(context.Background(), "kubestellar/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues across pages, want 2", len(issues))
	}
	if issues[0].Number != 1 || issues[1].Number != 2 {
		t.Errorf("paginated issues = %d, %d", issues[0].Number, issues[1].Number)
	}
}

func TestGitLabErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "kubestellar")
	_, err := f.GetRepo(context.Background(), "kubestellar/missing")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestGitLabProjectSlug(t *testing.T) {
	tests := []struct {
		name string
		org  string
		repo string
		want string
	}{
		{"bare name with org", "kubestellar", "hive", "kubestellar/hive"},
		{"already namespaced", "kubestellar", "other/hive", "other/hive"},
		{"nested namespace", "kubestellar", "group/sub/proj", "group/sub/proj"},
		{"bare name no org", "", "hive", "hive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &gitLabForge{org: tt.org}
			if got := f.projectSlug(tt.repo); got != tt.want {
				t.Errorf("projectSlug(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}
