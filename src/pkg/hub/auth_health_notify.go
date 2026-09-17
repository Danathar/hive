package hub

import (
	"fmt"
	"os"
	"strings"
)

// notifyOwnerAuthHealthDown DMs the hive owner when the fleet-wide
// backend-auth canary (#6558) transitions to down. Every skip is logged,
// never silent — same discipline as notifyOwnerAccessRequest beside it.
func (s *HubServer) notifyOwnerAuthHealthDown(entry RegistryEntry) {
	owner := strings.TrimSpace(entry.Owner)
	if owner == "" {
		// An unclaimed placeholder, or a bare/BYO hive with no owner on
		// record, has no one to notify.
		s.logger.Warn("auth-health-down notification skipped: hive has no owner on record",
			"hive", entry.ID, "reason", entry.AuthHealthReason)
		return
	}
	token := strings.TrimSpace(os.Getenv(slackTokenEnvVar))
	if token == "" {
		s.logger.Warn("auth-health-down notification skipped: no slack bot token configured",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}
	u := loadSaaSUser(owner)
	if u == nil {
		s.logger.Warn("auth-health-down notification skipped: owner has no user record",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}
	recipients, _ := resolveSlackRecipients([]SaaSUser{*u})
	if len(recipients) == 0 {
		s.logger.Warn("auth-health-down notification skipped: owner has no slack_id",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}

	message := fmt.Sprintf(
		"🚨 Hive %s: every enabled agent has failed backend auth for over %s\n%s\nOpen the hive dashboard: %s",
		entry.ID, AuthHealthDownThreshold().String(), entry.AuthHealthReason, hubDashboardBaseURL()+"/dashboard?manage_access="+entry.ID)

	s.logger.Warn("audit: auth-health-down notification queued",
		"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
	go s.deliverSlackMessages(token, "auth-health-down", message, recipients, "hive-auth-canary")
}
