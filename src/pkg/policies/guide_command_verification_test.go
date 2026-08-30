package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func TestGuidePoliciesRequireCommandVerification(t *testing.T) {
	t.Parallel()

	policyNames := []string{
		"guide-advisory.md",
		"guide-issues.md",
		"guide-holdgated.md",
		"guide-full.md",
		"guide.md",
	}
	required := [][]byte{
		[]byte("## Command Verification (MANDATORY)"),
		[]byte("Before writing, publishing, or proposing any shell command"),
		[]byte("authoritative registry or vendor source"),
		[]byte("exact spelling, case, version, repository/channel, and platform availability"),
		[]byte("Verify commands end to end"),
		[]byte("run every copy-pasteable command in a representative environment"),
		[]byte("validate the complete command against current authoritative documentation"),
		[]byte("Document prerequisites first"),
		[]byte("Do not present a dependent command as a first step"),
		[]byte("Record the evidence"),
		[]byte("if a command or artifact cannot be verified, do not publish it as working"),
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
			for sourceName, policy := range map[string][]byte{
				"embedded": embedded,
				"source":   source,
			} {
				for _, marker := range required {
					if !bytes.Contains(policy, marker) {
						t.Errorf("%s policy is missing command-verification guardrail %q", sourceName, marker)
					}
				}
			}
		})
	}
}
