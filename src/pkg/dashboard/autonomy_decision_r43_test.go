package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
)

func TestAutonomyDecisionSinkNilSafe(t *testing.T) {
	var nilSink *AutonomyDecisionSink
	nilSink.RecordAutonomyDecision(retro.AutonomyDecision{Repo: "o/r"})
	NewAutonomyDecisionSink(nil, nil, nil).RecordAutonomyDecision(retro.AutonomyDecision{Repo: "o/r"})
}

func TestAutonomyDecisionSinkRecordsAdvisoryAndCallbacks(t *testing.T) {
	s := newTestServer()
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "retro-beads"))
	if err != nil {
		t.Fatal(err)
	}
	s.deps = &Dependencies{BeadStores: map[string]*beads.Store{retro.Actor: store}}

	var gotTitle, gotMsg string
	var gotDemote bool
	var appliedRepo string
	var appliedLevel int
	sink := NewAutonomyDecisionSink(s,
		func(title, message string, demote bool) { gotTitle, gotMsg, gotDemote = title, message, demote },
		func(repo string, level int) { appliedRepo, appliedLevel = repo, level },
	)
	sink.RecordAutonomyDecision(retro.AutonomyDecision{
		Repo: "hivecommons/hive", Direction: "demote", From: 5, To: 4,
		EvidenceIDs: []string{"b-1", "b-2"}, Reason: "two reverted merges",
	})

	if gotTitle != "Automatic ACMM demote" || !gotDemote {
		t.Fatalf("notify = %q demote=%v", gotTitle, gotDemote)
	}
	if gotMsg != "hivecommons/hive moved L5 → L4 (two reverted merges)" {
		t.Fatalf("message = %q", gotMsg)
	}
	if appliedRepo != "hivecommons/hive" || appliedLevel != 4 {
		t.Fatalf("apply = %s L%d", appliedRepo, appliedLevel)
	}
	all := store.List(beads.ListFilter{})
	if len(all) != 1 {
		t.Fatalf("beads = %d, want 1", len(all))
	}
	b := all[0]
	if b.Title != "autonomy decision: hivecommons/hive demoted to L4" {
		t.Fatalf("title = %q", b.Title)
	}
	if b.Priority != beads.PriorityHigh {
		t.Fatalf("priority = %v, want high for demote", b.Priority)
	}
	for k, want := range map[string]string{
		"autonomy_decision":     "demote",
		"autonomy_scope_value":  "hivecommons/hive",
		"autonomy_from_level":   "5",
		"autonomy_to_level":     "4",
		"autonomy_evidence_ids": "b-1,b-2",
		"detail":                "two reverted merges",
	} {
		if got := b.Metadata[k]; got != want {
			t.Fatalf("metadata[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestAutonomyDecisionSinkPromoteWithoutStore(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{BeadStores: map[string]*beads.Store{}}
	notified := false
	NewAutonomyDecisionSink(s, func(_, _ string, demote bool) { notified = !demote }, nil).
		RecordAutonomyDecision(retro.AutonomyDecision{Repo: "o/r", Direction: "promote", From: 3, To: 4})
	if !notified {
		t.Fatal("promote should notify with demote=false")
	}
}

func TestActiveSwarmRepoWithoutStore(t *testing.T) {
	s := newTestServer()
	if got := s.ActiveSwarmRepo(); got != "" {
		t.Fatalf("ActiveSwarmRepo = %q, want empty", got)
	}
}

func TestChatGovernorAnswerHealthStates(t *testing.T) {
	s := newTestServer()
	s.status = nil
	if got := s.chatGovernorAnswer(); !strings.Contains(got, "still initializing") {
		t.Fatalf("nil status answer = %q", got)
	}
	s.status = &StatusPayload{DeepHealth: map[string]interface{}{"status": " degraded "}}
	if got := s.chatGovernorAnswer(); !strings.Contains(got, "hive health is degraded") {
		t.Fatalf("status answer = %q", got)
	}
	s.status = &StatusPayload{DeepHealth: map[string]interface{}{"ready": true}}
	if got := s.chatGovernorAnswer(); !strings.Contains(got, "hive health is ready") {
		t.Fatalf("ready answer = %q", got)
	}
	s.status = &StatusPayload{}
	if got := s.chatGovernorAnswer(); !strings.Contains(got, "hive health is unknown") {
		t.Fatalf("unknown answer = %q", got)
	}
	if got := s.chatSpekAnswer(); !strings.Contains(got, "not available yet") {
		t.Fatalf("spek answer = %q", got)
	}
}

func TestR43SmallHelpers(t *testing.T) {
	if got := firstNonEmptyString("", " ", "x", "y"); got != " " {
		t.Fatalf("firstNonEmptyString = %q", got)
	}
	if got := firstNonEmptyString("", ""); got != "" {
		t.Fatalf("firstNonEmptyString empty = %q", got)
	}
	if got := standbyPRNumber("https://github.com/o/r/pull/42"); got != 42 {
		t.Fatalf("standbyPRNumber = %d", got)
	}
	if got := standbyPRNumber("no-slash"); got != 0 {
		t.Fatalf("standbyPRNumber no slash = %d", got)
	}
	if got := providerPrefix("openai/gpt-4"); got != "openai/" {
		t.Fatalf("providerPrefix = %q", got)
	}
	if got := providerPrefix("gpt-4"); got != "" {
		t.Fatalf("providerPrefix bare = %q", got)
	}
	dst := map[string]string{"keep": "v"}
	mergeTeamMap(dst, map[string]string{" OS ": "linux", "": "skip", "blank": "   "})
	if dst["os"] != "linux" || dst["keep"] != "v" || len(dst) != 2 {
		t.Fatalf("mergeTeamMap = %v", dst)
	}
	now := time.Now()
	if got := (runLeaseSnapshot{stageStarted: now}).activityTime(); !got.Equal(now) {
		t.Fatal("activityTime should prefer stageStarted")
	}
	if got := (runLeaseSnapshot{expiresAt: now}).activityTime(); !got.Equal(now) {
		t.Fatal("activityTime should fall back to expiresAt")
	}
}

func TestRunDashboardURLAndNeedsDecisionLabel(t *testing.T) {
	var nilSrv *Server
	if got := nilSrv.runDashboardURL("r"); got != "" {
		t.Fatalf("nil server url = %q", got)
	}
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	if got := s.runDashboardURL("r"); got != "" {
		t.Fatalf("no public url = %q", got)
	}
	s.deps.Config.Dashboard.PublicURL = "https://hub.example/"
	if got := s.runDashboardURL(" run one "); got != "https://hub.example/runs/run%20one" {
		t.Fatalf("url = %q", got)
	}
	var nilHub *ContributeWSHub
	if got := nilHub.configuredNeedsDecisionLabel(); got == "" {
		t.Fatal("default needs-decision label should be non-empty")
	}
	h := &ContributeWSHub{server: s}
	if got := h.configuredNeedsDecisionLabel(); got == "" {
		t.Fatal("configured needs-decision label should be non-empty")
	}
}
