package apiproxy

import (
	"testing"

	"github.com/hivecommons/hive/crux/proxy"
)

// TestForgeRulesContract verifies the table-driven allow/deny rules across
// GitHub, GitLab, and Gitea at every agent autonomy level.
func TestForgeRulesContract(t *testing.T) {
	proxy.RunForgeRulesContractTest(t)
}
