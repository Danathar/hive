package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/github"
)

// publishFleetReports writes the fleet self-report results computed for the
// status payload upstream, unless the feature is in dry-run (the default).
func publishFleetReports(ctx context.Context, logger *slog.Logger, ghClient *github.Client, dashSrv *dashboard.Server, res *fleetreport.Result, dryRun bool) {
	if res == nil || dryRun || ghClient == nil || dashSrv == nil {
		return
	}
	for _, report := range res.Reports {
		write, err := ghClient.EnsureFleetReport(ctx, report)
		if err != nil {
			logger.Warn("fleet report: upstream write failed", "fingerprint", report.Fingerprint, "error", err)
			continue
		}
		dashSrv.MarkFleetReportPosted(report.Fingerprint, write.Number, write.URL, write.Created, report.Body)
		logger.Info("fleet report: upstream report recorded", "fingerprint", report.Fingerprint, "issue", write.Number, "created", write.Created, "commented", write.Commented, "reaction", write.ReactionSent)
	}
	for _, report := range res.Recoveries {
		open, ok := dashSrv.FleetReportOpenIssue(report.Fingerprint)
		if !ok {
			continue
		}
		if open.Number <= 0 {
			issue, found, err := ghClient.FleetReportIssue(ctx, report.Fingerprint)
			if err != nil {
				logger.Warn("fleet report: recovery lookup failed", "fingerprint", report.Fingerprint, "error", err)
				continue
			}
			if !found {
				dashSrv.ClearFleetReportOpen(report.Fingerprint)
				continue
			}
			open.Number = issue.Number
			open.URL = issue.URL
		}
		if err := ghClient.PostFleetReportRecovery(ctx, open.Number, report, open.OpenedByHive); err != nil {
			logger.Warn("fleet report: recovery write failed", "fingerprint", report.Fingerprint, "issue", open.Number, "error", err)
			continue
		}
		dashSrv.MarkFleetReportRecovered(report.Fingerprint)
		logger.Info("fleet report: recovery recorded", "fingerprint", report.Fingerprint, "issue", open.Number, "closed", open.OpenedByHive)
	}
}
