package dashboard

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestContributorStandingUsesRealTierRules(t *testing.T) {
	newcomer := contributorStandingForProfile(&ContributorProfile{
		TrustTier: "newcomer", TasksCompleted: 4, TasksWithPR: 2, TasksFailed: 3,
	})
	if newcomer.TrustTier != "newcomer" || newcomer.TasksCompleted != 4 || newcomer.TasksFailed != 3 {
		t.Fatalf("standing lost profile counters: %+v", newcomer)
	}
	if p := newcomer.TierProgress; p == nil || p.NextTier != "contributor" ||
		p.PRTasksRequired != contributorAutoPromoteAt || p.PRTasksRemaining != 3 || !p.Automatic || p.MaintainerGrantRequired {
		t.Fatalf("newcomer progress does not describe the real automatic rule: %+v", p)
	}

	contributor := contributorStandingForProfile(&ContributorProfile{
		TrustTier: "contributor", TasksWithPR: 7,
	})
	if p := contributor.TierProgress; p == nil || p.NextTier != "trusted" ||
		p.PRTasksRequired != contributorTrustedAt || p.PRTasksRemaining != 13 || p.Automatic || !p.MaintainerGrantRequired {
		t.Fatalf("contributor progress falsely describes trusted as automatic: %+v", p)
	}
}

func standingTestHub() *ContributeWSHub {
	return &ContributeWSHub{
		failedTasks:         make(map[string]time.Time),
		consecutiveFailures: make(map[string]int),
		logger: slog.New(slog.NewTextHandler(os.Stderr,
			&slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestContributorFailureNoticeSurfacesCooldownCauseAndStanding(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	task := &WSTaskAssign{Repo: "hivecommons/hive", Number: 6450}
	key := task.identityKey()
	hub := standingTestHub()
	hub.failedTasks[key] = now.Add(-30 * time.Second)
	hub.consecutiveFailures[key] = 1
	standing := contributorStandingForProfile(&ContributorProfile{
		TrustTier: "newcomer", TasksFailed: 1,
	})
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz123456"

	notice := hub.contributorFailureNotice(standing, task, ContributorFailure{
		Kind:   TaskFailureKindEnvironment,
		Reason: "Provider is not configured; token=" + secret,
	}, now)
	if notice == nil || notice.Type != "notice" {
		t.Fatalf("failure did not produce a notice: %+v", notice)
	}
	cd := notice.FailureCooldown
	if cd == nil || cd.State != "cooldown" || cd.TaskKey != key || cd.ConsecutiveFailures != 1 {
		t.Fatalf("notice lost the booked cooldown: %+v", cd)
	}
	if cd.RetryAfterSeconds != int64(failedTaskCooldownMinutes*60-30) {
		t.Errorf("retry_after_seconds = %d, want %d", cd.RetryAfterSeconds, failedTaskCooldownMinutes*60-30)
	}
	wantExpiry := now.Add(time.Duration(failedTaskCooldownMinutes)*time.Minute - 30*time.Second).Format(time.RFC3339)
	if cd.ExpiresAt != wantExpiry {
		t.Errorf("expires_at = %q, want %q", cd.ExpiresAt, wantExpiry)
	}
	if strings.Contains(cd.FailureReason, secret) || strings.Contains(notice.Message, secret) {
		t.Fatal("client-visible failure notice leaked a token")
	}
	for _, want := range []string{"10-minute failure cooldown", "2 more failures", "environment", "Provider is not configured", "Your standing", "0/5 PR tasks", "1 failed"} {
		if !strings.Contains(notice.Message, want) {
			t.Errorf("notice missing %q: %s", want, notice.Message)
		}
	}
}

func TestContributorFailureNoticeNamesQuarantine(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	task := &WSTaskAssign{Repo: "hivecommons/hive", Number: 6450}
	key := task.identityKey()
	hub := standingTestHub()
	hub.failedTasks[key] = now
	hub.consecutiveFailures[key] = permanentFailureWeight

	notice := hub.contributorFailureNotice(nil, task, ContributorFailure{
		Kind: TaskFailureKindEnvironment, Permanent: true,
	}, now)
	if notice == nil || notice.FailureCooldown == nil {
		t.Fatalf("permanent failure did not produce cooldown detail: %+v", notice)
	}
	if got := notice.FailureCooldown.State; got != "quarantine" {
		t.Fatalf("state = %q, want quarantine", got)
	}
	if notice.FailureCooldown.RetryAfterSeconds != int64(quarantineCooldownHours*60*60) {
		t.Errorf("retry_after_seconds = %d, want %d",
			notice.FailureCooldown.RetryAfterSeconds, quarantineCooldownHours*60*60)
	}
	if !strings.Contains(notice.Message, "quarantined for 6h") {
		t.Errorf("human notice does not name quarantine: %s", notice.Message)
	}
}
