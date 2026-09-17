package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/sessionprune"
)

// sessionPruneInterval is how often the janitor re-runs. Session directories
// accumulate at roughly 150-200/day on a busy spoke, so nothing is urgent here;
// this only needs to be frequent enough that a restart loop cannot outpace it.
const sessionPruneInterval = 6 * time.Hour

func runSessionPrune(logger *slog.Logger, dir string, maxAge time.Duration) {
	res, err := sessionprune.Prune(dir, maxAge, time.Now(), logger)
	if err != nil {
		logger.Warn("session prune failed", "dir", dir, "error", err)
		return
	}
	// Only speak up when something actually happened. A steady-state spoke
	// prunes nothing on most passes, and a line every 6 hours saying "removed 0"
	// is noise that trains operators to ignore the janitor.
	if res.Removed > 0 || res.Failed > 0 {
		logger.Info("session prune complete",
			"dir", dir,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_days", int(maxAge.Hours()/24))
	}
}
