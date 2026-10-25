package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

// workflowPermissionRefusal is GitHub's 403 body when update-branch (or the
// merges API) would bring .github/workflows/ changes into a head and the App
// lacks the workflows permission (projectbluefin/testsuite#991, 2026-10-25).
const workflowPermissionRefusal = "refusing to allow a GitHub App to create or update workflow `.github/workflows/docs-validate.yml` without `workflows` permission"

func TestClassifyUpdateBranchError(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	tests := []struct {
		name string
		err  error
		fork bool
		want updateBranchFailure
	}{
		{"merge conflict", resp(http.StatusUnprocessableEntity, "merge conflict between base and head"), false, updateBranchConflict},
		{"unrecognised 422 stays conflict", resp(http.StatusUnprocessableEntity, "Validation failed"), false, updateBranchConflict},
		{"already up to date", resp(http.StatusUnprocessableEntity, "There are no new commits on the base branch."), false, updateBranchUpToDate},
		{"already up to date legacy text", resp(http.StatusUnprocessableEntity, "branch is already up to date"), false, updateBranchUpToDate},
		{"403 on fork PR", resp(http.StatusForbidden, "Resource not accessible by integration"), true, updateBranchForkNotEditable},
		{"403 on same-repo PR", resp(http.StatusForbidden, "Resource not accessible by integration"), false, updateBranchOther},
		{"422 maintainer edits", resp(http.StatusUnprocessableEntity, "Maintainer edits are not allowed on this pull request"), false, updateBranchForkNotEditable},
		{"422 permission", resp(http.StatusUnprocessableEntity, "Insufficient permission to update head branch"), false, updateBranchForkNotEditable},
		{"403 workflow permission on same-repo PR", resp(http.StatusForbidden, workflowPermissionRefusal), false, updateBranchWorkflowPermission},
		{"403 workflow permission on fork PR", resp(http.StatusForbidden, workflowPermissionRefusal), true, updateBranchWorkflowPermission},
		{"403 workflow permission without backticks", resp(http.StatusForbidden, "refusing to allow a GitHub App to create or update workflow .github/workflows/ci.yml without workflows permission"), false, updateBranchWorkflowPermission},
		{"403 workflow permission wrapped by the mutation ledger", fmt.Errorf("updating branch: effect outcome uncertain, reconciliation required: %w", resp(http.StatusForbidden, workflowPermissionRefusal)), false, updateBranchWorkflowPermission},
		{"403 workflow permission marked not applied", effects.NotApplied(resp(http.StatusForbidden, workflowPermissionRefusal)), false, updateBranchWorkflowPermission},
		{"403 rate limit on same-repo PR", resp(http.StatusForbidden, "API rate limit exceeded for installation ID 1."), false, updateBranchOther},
		{"403 rate limit on fork PR", resp(http.StatusForbidden, "API rate limit exceeded for installation ID 1."), true, updateBranchForkNotEditable},
		{"server error", resp(http.StatusInternalServerError, "boom"), false, updateBranchOther},
		{"non-API error", errors.New("dial tcp"), false, updateBranchOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyUpdateBranchError(tt.err, tt.fork); got != tt.want {
				t.Fatalf("classifyUpdateBranchError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUpdateBranchAndSkipWorkflowPermission: the sweep reports GitHub's
// workflows-permission refusal as its own reason with no error, which stages
// 085, instead of "update-branch" plus the error (a retry every tick).
func TestUpdateBranchAndSkipWorkflowPermission(t *testing.T) {
	const head = "sha-head"
	cases := []struct {
		name       string
		message    string
		fork       bool
		wantReason string
		wantErr    bool
	}{
		{name: "same-repo workflow refusal", message: workflowPermissionRefusal, wantReason: autoMergeReasonWorkflowPermission},
		{name: "fork workflow refusal", message: workflowPermissionRefusal, fork: true, wantReason: autoMergeReasonWorkflowPermission},
		{name: "other same-repo 403 keeps update-branch", message: "Resource not accessible by integration", wantReason: "update-branch", wantErr: true},
		{name: "other fork 403 stays fork-not-editable", message: "Resource not accessible by integration", fork: true, wantReason: autoMergeReasonForkNotEditable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			updates := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					updates++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintf(w, `{"message":%q}`, tc.message)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
					_, _ = fmt.Fprintf(w, `{"number":7,"head":{"sha":%q}}`, head)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			event, reason, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 7, head, "behind", tc.fork)
			if reason != tc.wantReason || (err != nil) != tc.wantErr {
				t.Fatalf("reason, err = %q, %v; want %q, wantErr %v", reason, err, tc.wantReason, tc.wantErr)
			}
			if event.BranchUpdated {
				t.Fatal("a refused update must not report BranchUpdated")
			}
			mu.Lock()
			defer mu.Unlock()
			if updates != 1 {
				t.Fatalf("update-branch calls = %d, want 1", updates)
			}
		})
	}
}
