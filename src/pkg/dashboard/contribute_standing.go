package dashboard

import (
	"fmt"
	"strings"
	"time"
)

// contributorStandingForProfile copies only the non-secret profile fields a
// contributor needs to understand their own standing. Callers that share a
// live ContributorConnection must hold its mutex while copying the profile.
func contributorStandingForProfile(p *ContributorProfile) *ContributorStanding {
	if p == nil {
		return nil
	}
	standing := &ContributorStanding{
		TrustTier:      p.TrustTier,
		TasksCompleted: p.TasksCompleted,
		TasksWithPR:    p.TasksWithPR,
		TasksFailed:    p.TasksFailed,
	}
	standing.TierProgress = contributorTierProgress(p.TrustTier, p.TasksWithPR)
	return standing
}

func contributorTierProgress(tier string, prTasks int) *ContributorTierProgress {
	progress := &ContributorTierProgress{PRTasksCompleted: prTasks}
	switch tier {
	case "newcomer", "":
		progress.NextTier = "contributor"
		progress.PRTasksRequired = contributorAutoPromoteAt
		progress.Automatic = true
	case "contributor":
		progress.NextTier = "trusted"
		progress.PRTasksRequired = contributorTrustedAt
		progress.MaintainerGrantRequired = true
	case "trusted":
		progress.NextTier = "merger"
		progress.MaintainerGrantRequired = true
	case "merger":
		progress.NextTier = "advisor"
		progress.MaintainerGrantRequired = true
	default:
		return nil
	}
	if progress.PRTasksRequired > prTasks {
		progress.PRTasksRemaining = progress.PRTasksRequired - prTasks
	}
	return progress
}

func contributorStandingSummary(standing *ContributorStanding) string {
	if standing == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("tier %s", standing.TrustTier)}
	if p := standing.TierProgress; p != nil {
		switch {
		case p.Automatic:
			parts = append(parts, fmt.Sprintf("%d/%d PR tasks toward automatic %s promotion",
				p.PRTasksCompleted, p.PRTasksRequired, p.NextTier))
		case p.PRTasksRequired > 0:
			parts = append(parts, fmt.Sprintf("%d/%d PR tasks toward %s eligibility (maintainer grant required)",
				p.PRTasksCompleted, p.PRTasksRequired, p.NextTier))
		case p.MaintainerGrantRequired:
			parts = append(parts, fmt.Sprintf("next tier %s requires a maintainer grant", p.NextTier))
		}
	}
	parts = append(parts, fmt.Sprintf("%d completed, %d failed", standing.TasksCompleted, standing.TasksFailed))
	return strings.Join(parts, "; ")
}

// contributorFailureNotice snapshots the cooldown/quarantine that
// recordTaskFailureForTask just committed and turns it into both structured
// protocol data and prose an existing relay can display. now is injected so the
// countdown/expiry relationship is deterministic in tests.
func (h *ContributeWSHub) contributorFailureNotice(standing *ContributorStanding, task *WSTaskAssign, failure ContributorFailure, now time.Time) *WSMessage {
	if h == nil || task == nil {
		return nil
	}
	key := task.identityKey()
	if key == "" {
		return nil
	}

	h.completedMu.Lock()
	failedAt, ok := h.failedTasks[key]
	failures := h.consecutiveFailures[key]
	window := h.failureCooldownForLocked(key)
	h.completedMu.Unlock()
	if !ok {
		return nil
	}

	expires := failedAt.Add(window)
	retryAfter := int64(expires.Sub(now) / time.Second)
	if expires.After(now) && expires.Sub(now)%time.Second != 0 {
		retryAfter++
	}
	if retryAfter < 0 {
		retryAfter = 0
	}

	state := "cooldown"
	if failures >= consecutiveFailureQuarantineThreshold {
		state = "quarantine"
	}
	reason := truncateFailureReason(redactTokens(failure.Reason))
	cooldown := &ContributorFailureCooldown{
		TaskKey:             key,
		State:               state,
		ConsecutiveFailures: failures,
		QuarantineAt:        consecutiveFailureQuarantineThreshold,
		RetryAfterSeconds:   retryAfter,
		ExpiresAt:           expires.UTC().Format(time.RFC3339),
		FailureKind:         failure.Kind,
		FailureReason:       reason,
		Permanent:           failure.Permanent,
	}

	var message string
	if state == "quarantine" {
		message = fmt.Sprintf("Task %s is quarantined for %dh until %s (failure score %d/%d).",
			key, quarantineCooldownHours, cooldown.ExpiresAt, failures, consecutiveFailureQuarantineThreshold)
	} else {
		remaining := consecutiveFailureQuarantineThreshold - failures
		message = fmt.Sprintf("Task %s is in a %d-minute failure cooldown until %s (failure score %d/%d; %d more %s quarantine it).",
			key, failedTaskCooldownMinutes, cooldown.ExpiresAt, failures,
			consecutiveFailureQuarantineThreshold, remaining, pluralFailure(remaining))
	}
	if reason != "" {
		if failure.Kind != "" && failure.Kind != TaskFailureKindUnspecified {
			message += fmt.Sprintf(" Cause: %s — %s.", failure.Kind, reason)
		} else {
			message += " Cause: " + reason + "."
		}
	}
	if summary := contributorStandingSummary(standing); summary != "" {
		message += " Your standing: " + summary + "."
	}

	return &WSMessage{
		Type:                "notice",
		Seq:                 h.nextSeq(),
		Message:             message,
		ContributorStanding: standing,
		FailureCooldown:     cooldown,
	}
}

func pluralFailure(n int) string {
	if n == 1 {
		return "failure"
	}
	return "failures"
}
