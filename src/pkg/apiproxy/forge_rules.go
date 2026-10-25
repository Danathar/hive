package apiproxy

import (
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/proxy"
)

// AllowedByMode reports whether an HTTP method and path on any supported forge
// (GitHub, GitLab, Gitea) is permitted for an agent running in the specified mode.
// Hard-denied routes (e.g. direct MR/PR creation and merge) and unmapped routes
// fail closed (return false).
func AllowedByMode(mode agent.AgentMode, method, path string) bool {
	return proxy.AllowedByMode(mode, method, path)
}

// AllowedByModeCaps reports whether an HTTP method and path on any supported forge
// is permitted for an agent with the given mode and orthogonal capabilities.
func AllowedByModeCaps(mode agent.AgentMode, caps agent.AgentCapabilities, method, path string) bool {
	return proxy.AllowedByModeCaps(mode, caps, method, path)
}

// DeniedMessage returns the agent-facing directive explaining why a request was
// blocked by a hard-deny rule (e.g. instructing to use the request-file path),
// and true if a hard-deny rule matched.
func DeniedMessage(method, path string) (string, bool) {
	return proxy.DeniedMessage(method, path)
}
