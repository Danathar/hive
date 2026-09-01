package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// These are the policy variants that can retry, repair, or escalate a failing
// PR check. Advisory/issues-only variants report findings but do not own PR
// repair, so they are intentionally outside this contract.
func TestFixPoliciesRequireSharedCIBaselineTriage(t *testing.T) {
	t.Parallel()

	policyNames := []string{
		"scanner.md",
		"scanner-full.md",
		"scanner-holdgated.md",
		"scanner-automerge.md",
		"ci-maintainer.md",
		"ci-maintainer-full.md",
		"ci-maintainer-holdgated.md",
		"quality.md",
		"quality-full.md",
		"quality-holdgated.md",
	}
	required := [][]byte{
		[]byte("## Shared CI Baseline Triage (MANDATORY)"),
		[]byte("hive-baseline-check.sh"),
		[]byte("<owner/repo from PR>"),
		[]byte("Exit `0`"),
		[]byte("exit `1`"),
		[]byte("exit `2`"),
		[]byte("one repository incident, not one failure per PR"),
		[]byte("[shared-ci] <check name> failing across <owner/repo>"),
		[]byte("Never repost an existing"),
	}

	for _, name := range policyNames {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			embedded, err := DefaultPolicies.ReadFile(path.Join("defaults", name))
			if err != nil {
				t.Fatalf("read embedded policy: %v", err)
			}
			source, err := os.ReadFile(filepath.Join("..", "..", "policies", name))
			if err != nil {
				t.Fatalf("read source policy: %v", err)
			}
			for variant, policy := range map[string][]byte{
				"embedded": embedded,
				"source":   source,
			} {
				for _, marker := range required {
					if !bytes.Contains(policy, marker) {
						t.Errorf("%s policy is missing shared-baseline guardrail %q", variant, marker)
					}
				}
			}
		})
	}
}
