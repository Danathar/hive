package main

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func syncAutoMergePolicyToGitHubClient(cfg *config.Config, ghClient *github.Client) (map[string]bool, bool) {
	if cfg == nil || ghClient == nil {
		return nil, false
	}
	set, ok := cfg.AutoMerge.RequiredCheckSet()
	// The merge-request watcher's pre-merge CI gate (#6173) names required
	// checks that have not reported yet, so it needs the same declared set the
	// sweep gates on. Passing nil clears stale values after config reload.
	ghClient.SetRequiredChecks(set)
	ghClient.SetMergeRequestAllowUnprotectedBaseRepos(cfg.AutoMerge.AllowUnprotectedBaseSet())
	ghClient.SetMergeRequestNoCIAllowedRepos(cfg.AutoMerge.NoCIOKSet())
	return set, ok
}
