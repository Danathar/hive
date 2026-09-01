package policies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutreachPolicyRequiresGroundedHumanReviewedClaims(t *testing.T) {
	t.Parallel()

	embedded, err := DefaultPolicies.ReadFile("defaults/outreach-full.md")
	if err != nil {
		t.Fatalf("read embedded outreach policy: %v", err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "policies", "outreach-full.md"))
	if err != nil {
		t.Fatalf("read source outreach policy: %v", err)
	}

	required := []string{
		"Every outreach PR requires the `hold` label, including at ACMM L6",
		"Ground every product capability claim before writing it",
		"cite the exact implementing file, test, release, or human-authored official documentation for each claim",
		"If a claim cannot be verified, omit it and flag the question for a human",
		"NEVER make regulatory or compliance claims",
		"Software features are not proof of an organization's compliance posture",
		"NEVER invent roadmap commitments",
		"## Claim evidence",
		"--label \"community,outreach,hold\"",
	}
	for _, policy := range []struct {
		name string
		body string
	}{
		{name: "embedded", body: string(embedded)},
		{name: "source", body: string(source)},
	} {
		t.Run(policy.name, func(t *testing.T) {
			for _, marker := range required {
				if !strings.Contains(policy.body, marker) {
					t.Errorf("outreach policy is missing safety requirement %q", marker)
				}
			}
			if strings.Contains(policy.body, "No hold label required") {
				t.Error("outreach policy still exempts public content from human review")
			}
		})
	}
}
