package scheduler

import (
	"os"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/escalation"
)

func TestFormatReviewerLaneDataCapsAndSkipsCompletedPass(t *testing.T) {
	data := []byte(`{"reviewer_queue":[
	  {"number":1,"repo":"acme/widgets","head_sha":"a","ci_status":"failure","failing_checks":["test"],"excerpt":"want 1 got 2"},
	  {"number":2,"repo":"acme/widgets","head_sha":"b","labels":["hive/reviewer-pass"],"failing_checks":["lint"]},
	  {"number":4,"repo":"acme/widgets","head_sha":"d","ci_status":"success","failing_checks":["dco"]}
	]}`)

	got := formatReviewerLaneData(data, 1, false)
	for _, want := range []string{"acme/widgets#1", "CI: failure", "cap 1", "RECOMMEND-CLOSE", "1 more remain queued", escalation.ReviewerPassLabel, "hive-review"} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer section missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"acme/widgets#2", "acme/widgets#4", "- CLOSE:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("reviewer section unexpectedly contains %q:\n%s", unwanted, got)
		}
	}
}

func TestFormatReviewerLaneDataL6AllowsClose(t *testing.T) {
	data := []byte(`{"reviewer_queue":[{"number":7,"repo":"acme/widgets","head_sha":"abc"}]}`)
	got := formatReviewerLaneData(data, 3, true)
	if !strings.Contains(got, "- CLOSE:") || strings.Contains(got, "RECOMMEND-CLOSE") {
		t.Fatalf("L6 close authority not rendered correctly:\n%s", got)
	}
}

func TestAddEscalatedReviewerLaneRequiresOptInLevelAndAgent(t *testing.T) {
	oldPath := ciFailingPath
	ciFailingPath = t.TempDir() + "/ci-failing.json"
	t.Cleanup(func() { ciFailingPath = oldPath })
	if err := os.WriteFile(ciFailingPath, []byte(`{"reviewer_queue":[{"number":7,"repo":"acme/widgets","head_sha":"abc"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	level := 5
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents:    map[string]config.AgentConfig{"reviewer": {Role: "reviewer"}, "scanner": {Role: "scanner"}},
		Escalation: config.EscalationConfig{Reviewer: config.EscalationReviewerConfig{
			Enabled: true,
		}},
	}
	s := New(cfg, nil)
	base := "[agent:reviewer]\nbase\n"
	if got := s.addEscalatedReviewerLane("reviewer", base); !strings.Contains(got, "REVIEWER LANE") {
		t.Fatalf("enabled L5 reviewer did not receive queue:\n%s", got)
	}
	if got := s.addEscalatedReviewerLane("scanner", base); got != base {
		t.Fatal("non-reviewer received escalated queue")
	}
	cfg.Agents["reviewer"] = config.AgentConfig{Role: "scanner"}
	if got := s.addEscalatedReviewerLane("reviewer", base); got != base {
		t.Fatal("configured agent without reviewer role received escalated queue")
	}
	cfg.Agents["reviewer"] = config.AgentConfig{Role: "reviewer"}
	cfg.Escalation.Reviewer.Enabled = false
	if got := s.addEscalatedReviewerLane("reviewer", base); got != base {
		t.Fatal("disabled reviewer lane still injected a queue")
	}
	cfg.Escalation.Reviewer.Enabled = true
	level = 4
	if got := s.addEscalatedReviewerLane("reviewer", base); got != base {
		t.Fatal("sub-L5 reviewer lane did not fail closed")
	}
}
