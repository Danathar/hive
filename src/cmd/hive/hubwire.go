package main

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/governor"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

func heartbeatBudgetState(govState governor.State) (*bool, *int) {
	if govState.LastEval.IsZero() {
		return nil, nil
	}
	exhausted := govState.BudgetExhausted
	violations := govState.SLAViolations
	return &exhausted, &violations
}

func freshHeartbeatHealthSummary(agents []spoke.AgentSummary) map[string]any {
	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	running := 0
	down := 0
	var downAgents []string
	for _, a := range agents {
		switch strings.ToLower(a.State) {
		case "running":
			running++
		case "stopped", "failed":
			if !a.Paused {
				down++
				downAgents = append(downAgents, a.Name)
			}
		}
	}
	checks := []check{{
		Name:   "heartbeat_stats",
		Status: "warn",
		Detail: "expensive heartbeat stats stale; agent state refreshed from memory",
	}}
	agentStatus := "pass"
	agentDetail := fmt.Sprintf("%d running", running)
	if down > 0 {
		agentStatus = "fail"
		agentDetail = fmt.Sprintf("%d running, %d down: %s", running, down, strings.Join(downAgents, ", "))
	}
	checks = append(checks, check{Name: "agents", Status: agentStatus, Detail: agentDetail})
	return map[string]any{
		"status": "unknown",
		"checks": checks,
	}
}
