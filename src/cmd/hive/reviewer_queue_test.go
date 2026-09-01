package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/escalation"
	"github.com/kubestellar/hive/pkg/github"
)

func TestWriteMergeEligibleReviewerQueueKeepsGreenAgentPRsAndExcludesHumans(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "widgets", Number: 1, Author: "hive-agent", Labels: []string{escalation.NeedsHumanLabel}, CIStatus: "success", HeadSHA: "green"},
		{Repo: "widgets", Number: 2, Author: "app[bot]", CIStatus: "failure", HeadSHA: "red", FailingChecks: []string{"test"}},
		{Repo: "widgets", Number: 3, Author: "alice", Labels: []string{escalation.NeedsHumanLabel}, CIStatus: "failure", HeadSHA: "human"},
		{Repo: "widgets", Number: 4, Author: "app[bot]", CIStatus: "success", HeadSHA: "ordinary"},
	}}}
	escalated := map[string]bool{escalation.Key("acme/widgets", 2): true}
	writeMergeEligible(actionable, github.HoldResult{}, "acme", "hive-agent", escalated, false, nil, false, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw, err := os.ReadFile(ciFailingPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ReviewerQueue []struct {
			Number   int    `json:"number"`
			CIStatus string `json:"ci_status"`
		} `json:"reviewer_queue"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.ReviewerQueue) != 2 {
		t.Fatalf("reviewer_queue = %+v, want two agent-authored escalations", payload.ReviewerQueue)
	}
	if payload.ReviewerQueue[0].Number != 1 || payload.ReviewerQueue[0].CIStatus != "success" {
		t.Fatalf("green needs-human PR missing from reviewer_queue: %+v", payload.ReviewerQueue)
	}
	if payload.ReviewerQueue[1].Number != 2 || payload.ReviewerQueue[1].CIStatus != "failure" {
		t.Fatalf("newly escalated bot PR missing from reviewer_queue: %+v", payload.ReviewerQueue)
	}
}
