package main

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
)

// sentinelClearApprovalAuditAction is the audit entry written when a trusted
// merger's removal of the sentinel label is recorded as their approval.
const sentinelClearApprovalAuditAction = "sentinel-clear-approval"

// sentinelClearApprovalStore resolves the decision ledger the approval is
// recorded in: the one a dashboard Approve and `/hive decision approve`
// write to. Tests swap in a memory-only store.
var sentinelClearApprovalStore = func() review.HumanChangeRequestStore {
	return review.DefaultHumanChangeRequests()
}

// sentinelClearApprovalFunc is SentinelSweepOptions.ClearApproval for this
// hive: nil while sentinel.clear_counts_as_approval is off (the default), so
// the sweep behaves exactly as before. On, it applies the trusted-merger
// test the merge lanes use (trustedMergerFunc, through
// automerge.RecordSentinelClearApproval) to whoever removed the label, and
// writes one audit entry the first time a removal is recorded.
func sentinelClearApprovalFunc(cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server) func(github.SentinelClearance) github.SentinelClearApproval {
	if cfg == nil || !cfg.Sentinel.ClearCountsAsApproval {
		return nil
	}
	authz := trustedMergerFunc(cfg)
	return func(cl github.SentinelClearance) github.SentinelClearApproval {
		out := automerge.RecordSentinelClearApproval(sentinelClearApprovalStore(), authz, ghClient.AppBotLogin(), cl)
		if out.Recorded && dashSrv != nil {
			detail := fmt.Sprintf("repo=%s, pr=%d, head=%s, by=%s, label=%s, label_event=%d", cl.Repo, cl.Number, cl.HeadSHA, cl.Actor, cl.Label, cl.EventID)
			// Recorded by the sweep, so the user is "system": naming the
			// remover there would count as their dashboard activity. The
			// detail carries who it was.
			dashSrv.AuditLogRecord("system", sentinelClearApprovalAuditAction, detail, "", cl.Repo, cl.Number)
		}
		return out
	}
}
