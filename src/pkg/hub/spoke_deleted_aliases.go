package hub

import (
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func QuotaExhaustedProcessCount(statuses map[string]*agent.AgentProcess) int {
	return spoke.QuotaExhaustedProcessCount(statuses)
}

func QuotaExhaustedAgentReason(count int) string { return spoke.QuotaExhaustedAgentReason(count) }
