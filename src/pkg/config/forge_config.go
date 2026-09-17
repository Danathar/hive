// Forge identity and connection configuration: GitHubConfig (App/OAuth/GHE
// resolution), GitLabConfig, GiteaConfig, and the known-forge identity table.
package config

// AppSignedCommitsEnabled reports whether the PR-request watcher re-authors
// agent branches through createCommitOnBranch so their commits are GitHub-
// signed. Opt-in: nil and false both mean off. See AppSignedCommits.
func (g GitHubConfig) AppSignedCommitsEnabled() bool {
	return g.AppSignedCommits != nil && *g.AppSignedCommits
}

// SelfAuthorizationHoldEnabled reports whether the #5117 self-authorization
// hold is active for this hive. Default ON preserves the existing policy for
// every hive that has not explicitly opted out.
func (g GitHubConfig) SelfAuthorizationHoldEnabled() bool {
	if g.selfAuthorizationHoldEnvOverride != nil {
		return *g.selfAuthorizationHoldEnvOverride
	}
	if g.SelfAuthorizationHold == nil {
		return true
	}
	return *g.SelfAuthorizationHold
}

// SelfAuthorizationHoldEnvOverrideSet reports whether
// HIVE_SELF_AUTHORIZATION_HOLD is currently forcing the effective value.
func (g GitHubConfig) SelfAuthorizationHoldEnvOverrideSet() bool {
	return g.selfAuthorizationHoldEnvOverride != nil
}
