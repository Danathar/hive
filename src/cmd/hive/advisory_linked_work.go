package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/github"
)

func enrichAdvisoryLinkedWork(ctx context.Context, client *github.Client, digest *advisory.Digest, owner, repo string, logger *slog.Logger) {
	// Bound the entire enrichment pass, including a slow or rate-limited API,
	// so advisory display metadata cannot stall the evaluation loop.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	advisory.ResolveLinkedWork(digest, owner, repo, func(owner, repo string, number int) (advisory.LinkedWork, bool) {
		if client == nil || ctx.Err() != nil {
			return advisory.LinkedWork{}, false
		}
		work, err := client.AdvisoryWorkState(ctx, owner, repo, number)
		if err != nil {
			logger.Debug("advisory: linked work state unavailable", "owner", owner, "repo", repo, "number", number, "error", err)
		}
		return work, err == nil
	})
}
