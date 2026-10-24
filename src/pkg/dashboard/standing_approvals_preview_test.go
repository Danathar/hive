package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// project.standing_approvals is config-only, so the prompt editor's preview
// renders it where the kick will, per lane, and renders nothing when unset.

func TestSubstituteTemplateVars_StandingApprovalsRenderInThePreview(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.StandingApprovals = []config.StandingApproval{
		{ID: "advisory-ci-checks", Text: "Advisory CI jobs are approved."},
		{ID: "quality-only", Text: "Quality only.", Agents: []string{"quality"}},
	}

	out := s.substituteTemplateVars("Use the flag.${STANDING_APPROVALS}\n", "scanner")
	if strings.Contains(out, "${STANDING_APPROVALS}") {
		t.Fatalf("preview left ${STANDING_APPROVALS} unrendered:\n%s", out)
	}
	if !strings.Contains(out, "- `advisory-ci-checks`: Advisory CI jobs are approved.") {
		t.Fatalf("preview does not show the entry:\n%s", out)
	}
	if strings.Contains(out, "quality-only") {
		t.Fatalf("preview for scanner shows an entry scoped to quality:\n%s", out)
	}
}

func TestSubstituteTemplateVars_StandingApprovalsUnsetRenderNothing(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.StandingApprovals = nil

	if out := s.substituteTemplateVars("Use the flag.${STANDING_APPROVALS}\n", "scanner"); out != "Use the flag.\n" {
		t.Fatalf("no standing approvals must render nothing; got %q", out)
	}
}
