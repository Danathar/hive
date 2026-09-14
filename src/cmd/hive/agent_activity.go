package main

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
)

func agentActivityFor(mgr *agent.Manager, cfg *config.Config, govState governor.State, currentMode, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) hub.AgentActivity {
	act := hub.AgentActivity{
		Paused:         proc.Paused,
		PausedTrigger:  proc.PausedTrigger,
		PausedReason:   proc.PausedReason,
		PausedBy:       proc.PausedBy,
		PausedAt:       proc.PausedAt,
		NeedsLogin:     proc.NeedsLogin,
		QuotaExhausted: proc.QuotaExhausted,
		LastActivityAt: proc.LastPaneChange,
		SessionMissing: mgr.SessionMissing(name),
	}
	if status := proc.BackendAuth.Status; status != "" && status != agent.BackendAuthOK {
		act.BackendAuthStatus = status
		act.BackendAuthSince = proc.BackendAuth.Since
		act.BackendAuthLastError = proc.BackendAuth.LastError
	}
	if proc.StartedAt != nil {
		act.StartedAt = *proc.StartedAt
	}
	act.KickInterval = heartbeatKickInterval(govState, name, proc, onDemandFromPack)
	if cfg != nil {
		onDemandAgent := false
		enabled := false
		if ac, ok := cfg.Agents[name]; ok {
			onDemandAgent = ac.OnDemand
			enabled = ac.Enabled
		}
		act.ExpectedActive = cfg.ExpectedActive(name, currentMode, onDemandAgent, onDemandFromPack)
		act.Enabled = enabled
	}
	if canIssue, canPR, canMerge, ok := mgr.AgentCapabilities(name); ok {
		act.CanOpenIssue = canIssue
		act.CanOpenPR = canPR
		act.CanMerge = canMerge
	}
	if backend, ok := mgr.EffectiveBackend(name); ok {
		act.Backend = backend
	}
	if sf, ok := mgr.StartFailureState(name); ok && strings.TrimSpace(sf.Reason) != "" && sf.Count > 0 {
		act.StartFailureReason = sf.Reason
		act.StartFailureCount = sf.Count
		act.StartFailureLastAt = sf.LastAt
		act.StartBlocked = sf.Blocked
		if sf.Blocked {
			act.StartBlockedReason = sf.Reason
		}
		if sf.LastExitCode != nil {
			act.StartFailureExitCode = sf.LastExitCode
		}
		act.StartFailureSignal = sf.LastSignal
	}
	if total, last24h, lastAt, reason, ok := mgr.RestartTelemetry(name); ok {
		act.Restarts.Total = total
		act.Restarts.Last24h = last24h
		act.Restarts.LastReason = reason
		if !lastAt.IsZero() {
			act.Restarts.LastRestartAt = lastAt.UTC().Format(time.RFC3339)
		}
	}
	return act
}

func heartbeatKickInterval(govState governor.State, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) time.Duration {
	if proc == nil || !proc.Config.UsesGovernorKick() || proc.Config.OnDemand || onDemandFromPack[name] {
		return 0
	}
	interval := governorShortestActiveInterval(govState, name)
	return interval
}

func governorCadencesForAgent(govState governor.State, name string) []governor.AgentCadence {
	var cadences []governor.AgentCadence
	for key, cadence := range govState.Cadences {
		agent, _ := config.SplitCadenceTargetKey(key)
		if agent == name || cadence.Agent == name {
			cadences = append(cadences, cadence)
		}
	}
	return cadences
}

func governorLastKickForAgent(govState governor.State, name string) time.Time {
	var newest time.Time
	for key, ts := range govState.LastKick {
		agent, _ := config.SplitCadenceTargetKey(key)
		if agent == name && !ts.IsZero() && ts.After(newest) {
			newest = ts
		}
	}
	return newest
}

func governorShortestActiveInterval(govState governor.State, name string) time.Duration {
	var shortest time.Duration
	for _, cadence := range governorCadencesForAgent(govState, name) {
		if cadence.Paused || cadence.Interval <= 0 {
			continue
		}
		if shortest == 0 || cadence.Interval < shortest {
			shortest = cadence.Interval
		}
	}
	return shortest
}
