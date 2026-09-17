package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/github"
)

// GitHub App credential verdicts now live in pkg/apphealth (#7238 stage 1).
// These wrappers keep every existing call site and test unchanged; the only
// thing they add is the private-key paths, which the extracted code takes as
// a field instead of reading the package-level appKeys global.

// appKeyPaths snapshots the two App key path locations for a pkg/apphealth
// call. Read at call time on purpose: tests repoint these, and capturing them
// once would silently ignore that.
func appKeyPaths() apphealth.KeyPaths {
	return apphealth.KeyPaths{Spoke: appKeys.DataKeyPath, Provisioned: appKeys.ProvisionedKeyPath}
}

func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyFailure(ctx, appAuth, expectedOwner, appKeyPaths(), logger)
}

func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (string, github.AppAuthState) {
	return apphealth.ClassifyWriteForbidden(ctx, appAuth, expectedOwner, repo, appKeyPaths())
}

func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyRepoCoverage(ctx, appAuth, org, repos, logger)
}
