package spoke

import (
	"os"
	"strconv"
	"strings"
)

const (
	EnvAgentRestartProblemThreshold     = "HIVE_HUB_AGENT_RESTART_PROBLEM_THRESHOLD"
	DefaultAgentRestartProblemThreshold = 5
)

// AgentRestartProblemThreshold is the restart-storm bar: an agent that has
// restarted at least this many times in the rolling 24h window is a problem,
// not a quiet agent. It lives on the spoke side of the hub/spoke boundary so
// the spoke's own status builder (pkg/dashboard) can escalate a crash-looping
// agent with the SAME threshold the hub's fleet verdict applies — otherwise
// the local card and the fleet view could disagree about the same agent
// (#6237) — without pkg/dashboard depending on pkg/hub. The hub re-exports
// it as hub.AgentRestartProblemThreshold.
func AgentRestartProblemThreshold() int {
	if raw := strings.TrimSpace(os.Getenv(EnvAgentRestartProblemThreshold)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultAgentRestartProblemThreshold
}
