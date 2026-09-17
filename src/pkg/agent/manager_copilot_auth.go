// Copilot/Claude credential file plumbing: shared config token probes,
// durable copilot user token restore/promote, identity keying, and the
// copilot login diagnostic.
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// sharedCopilotConfigPath and sharedClaudeCredentialPath are vars (not consts)
// solely so tests can redirect them to temp files and exercise the config/token
// helpers (copilotConfigHasTokens, clearExpiredTokens, configHasTokens,
// fixSharedConfigPerms) without a real /data volume. Production values are
// unchanged; nothing on the launch path mutates them.
var (
	sharedCopilotConfigPath    = "/data/home/.copilot/config.json"
	sharedClaudeCredentialPath = "/data/home/.claude/.credentials.json"
)

const (
	sharedConfigDesiredMode = 0o660
	// agyDefaultEffort is the reasoning effort passed alongside agy's --model
	// when the agent has no usable reasoning_effort configured (see
	// agyLaunchEffort). agy requires --effort whenever --model is given and
	// otherwise ignores the model entirely; "low" is the effort agy defaults
	// to on its own, so this makes the configured model take effect without
	// changing behaviour.
	agyDefaultEffort = "low"

	tokenRestartCooldownSec = 60 // minimum seconds between token-triggered restarts per agent
	// loginPromptTailLines bounds the pane region the login-prompt detector
	// reads: a prompt the CLI is stuck at sits at the pane bottom, while
	// echoed kick text and startup flashes live in scrollback (see the poller).
	loginPromptTailLines = 15
	// loginStreakRestartMin is how many consecutive polls (~3s apart) must see
	// the login prompt before a token-triggered restart may fire — filters the
	// CLI's transient startup "/login" flash.
	loginStreakRestartMin = 3
	// tokenRestartMaxAttempts bounds CONSECUTIVE token-triggered restarts that
	// fail to clear the login prompt.
	//
	// The three guards above answer WHEN to restart; none of them answered HOW
	// MANY TIMES, so a restart that could never work was retried forever at the
	// cooldown interval. #4596 is precisely that shape: the shared credential is
	// valid (so configHasTokens() is true) while $HOME/.claude.json has lost its
	// oauthAccount (so the CLI shows the login menu regardless), and each
	// restart re-launched a CLI that rewrote the same contended file and asked
	// again. Restarts are not free — they destroy in-flight work, which is the
	// failure the kick grace above was added for.
	//
	// Three is deliberately generous: one restart genuinely does fix the case
	// this feature was built for (an operator authenticates in one agent's
	// terminal and the others need a nudge), so the cap only engages on a
	// theory that has now failed repeatedly.
	tokenRestartMaxAttempts = 3
	// tokenRestartKickGrace suppresses token-triggered restarts after a kick
	// delivery so the restart can never destroy just-delivered work.
	tokenRestartKickGrace      = 10 * time.Minute
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
	tlsErrorRestartCooldownSec = 120 // minimum seconds between TLS-error-triggered restarts per agent
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token; injected into agents as COPILOT_GITHUB_TOKEN.
const CopilotUserTokenPath = "/data/copilot-user-token"

var copilotUserTokenWatchPath = CopilotUserTokenPath

// copilotUserTokenProbePath is the same location as consulted by the
// AgentAuthState file probe. A var (not the const directly) purely as a TEST
// SEAM, matching sharedCopilotConfigPath above: on a live hive host the real
// /data/copilot-user-token exists, and a probe that cannot be redirected makes
// every "no copilot credentials" test assert against production login state.
var copilotUserTokenProbePath = CopilotUserTokenPath

// claudeCredentialReachable reports whether a usable Claude credential exists
// at the locations this agent's CLI will look — its per-UID home first, then
// the shared path its ~/.claude symlink resolves to.
//
// It is a REACHABILITY check, not a permission check, and the distinction is
// worth stating: this runs in the hive process, so it proves the file is there
// and parseable, not that the agent's UID can open it. The deployment keeps
// those the same — the entrypoint's inotify guard chowns /data/home/.claude to
// dev:node and holds it group-readable on every write, precisely so every
// agent UID can read it (#4619). If that ever drifts, an agent lands at a login
// prompt with no injected token instead of a working one; that is a loud,
// alerting state, not a silent one, which is the right direction to fail in.
//
// HasUsableToken, not HasValidToken: an access token that has aged out is
// exactly the case the CLI fixes for itself on start, by redeeming the refresh
// grant beside it. Treating that state as "no credential here" would re-inject
// the static override precisely when the CLI was about to recover, which is the
// failure this guard exists to prevent.
func claudeCredentialReachable(agent *AgentProcess, backend string) bool {
	if agent == nil {
		return false
	}
	for _, p := range agentClaudeCredentialPaths(agent.Name, agent.UID, backend) {
		if claude.HasUsableToken(p) {
			return true
		}
	}
	return false
}

// Diagnostic pacing. Vars, not consts, so the pkg/agent TestMain can shrink
// them (see the pacing block near deliverStartupKick). Production values
// unchanged.
var (
	diagnosticTimeoutSec = 20
	diagnosticPollSec    = 2
)

// authErrorPatterns indicate the stored token was DEFINITIVELY rejected by the
// server and should be cleared. These are server-side rejections, not CLI
// prompts. A bare interactive login/"re-authenticate" prompt is intentionally
// NOT here: on a slow cold start after an upgrade the Copilot CLI can surface a
// login/device-flow prompt while the token on disk is still valid, and clearing
// it there destroys a good token and forces the user to re-login on every
// upgrade. Only a genuine credential rejection purges the token.
var authErrorPatterns = []string{
	"Bad credentials",
	"401 Unauthorized",
	"token found but could not be validated",
	"Failed to fetch OAuth user login",
}

// matchesAuthError reports whether copilot diagnostic output shows a definitive
// server-side credential rejection that justifies purging the stored token. A
// bare login/"re-authenticate" prompt does NOT match — that is handled by
// paneShowsLoginPrompt (a non-destructive "needs login" UI signal) so a slow
// cold start after an upgrade cannot destroy a still-valid token.
func matchesAuthError(output string) bool {
	for _, pat := range authErrorPatterns {
		if strings.Contains(output, pat) {
			return true
		}
	}
	return false
}

func (m *Manager) runCopilotDiagnostic(ctx context.Context, agent *AgentProcess) {
	m.tmuxSendKeysForAgent(agent, "C-c", "")
	time.Sleep(paneCaptureSleep)
	// Only sweep by UID when isolation gave this agent a real per-agent UID.
	// agent.UID==0 (isolation off or agent missing from the UID map) would
	// otherwise ask killAgentProcesses to match root — the internal floor guard
	// blocks it, but skipping the call makes the intent explicit.
	if agent.UID > 0 {
		killAgentProcesses(agent.UID, m.logger)
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	if err := m.ensureTmuxSession(agent); err != nil {
		m.logger.Warn("diagnostic: failed to create tmux session", "agent", agent.Name, "error", err)
		return
	}

	binary, err := backendBinary("copilot")
	if err != nil {
		m.logger.Warn("diagnostic: copilot binary not found", "error", err)
		return
	}
	m.tmuxSendLiteralForAgent(agent, fmt.Sprintf("HOME=/data/home %s", binary))
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	m.logger.Info("diagnostic: launched bare copilot to capture error", "agent", agent.Name)

	deadline := time.After(time.Duration(diagnosticTimeoutSec) * time.Second)
	ticker := time.NewTicker(time.Duration(diagnosticPollSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			// The diagnostic already killed the agent's real session above, so
			// parking here would strand the agent dead in StateFailed with no
			// path back except an operator restart (live signature: quality,
			// scanner and sec-check on a 14h-old hive all "failed to start:
			// copilot hung with no output (diagnostic timed out)", every kick
			// refused for 15h while 245 issues queued). An inconclusive
			// diagnostic is not a start failure — relaunch, like the other two
			// branches do, and only fall back to StateFailed if the relaunch
			// itself fails.
			m.logger.Warn("diagnostic: timed out waiting for copilot error output, relaunching agent", "agent", agent.Name)
			agent.LastError = "copilot hung with no output (diagnostic timed out)"
			if agent.UID > 0 {
				killAgentProcesses(agent.UID, m.logger)
			}
			_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
			agent.forceRelaunch = true
			if err := m.RestartWithReason(ctx, agent.Name, "copilot hang diagnostic timed out"); err != nil {
				m.logger.Warn("diagnostic: restart after timeout failed", "agent", agent.Name, "error", err)
				agent.State = StateFailed
				m.audit(AuditAgentStartFailed, agent.Name, auditFields(
					"outcome", "failure",
					"backend", agent.effectiveBackend(),
					"model", agent.effectiveModel(),
					"error", agent.LastError,
				))
			}
			return
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			if matchesAuthError(output) {
				agent.LastError = "auth token expired or invalid"
				// Prefer to RESTORE the stored token over merely clearing it: an
				// empty copilotTokens leaves CLI 1.0.78 stuck at /login (it does
				// not re-populate from the injected env token), and every roll
				// re-hits this. If we hold a durable user token, seed it so the
				// relaunch below comes up authenticated; otherwise fall back to
				// the historical clear (which lets the CLI reach /login instead
				// of hanging on the MITM proxy on a stale token).
				m.mu.RLock()
				userTok := m.copilotAuthToken
				m.mu.RUnlock()
				if strings.TrimSpace(userTok) != "" {
					m.logger.Warn("diagnostic: auth error detected, restoring token from durable user token",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := restoreCopilotTokens(sharedCopilotConfigPath, userTok); err != nil {
						m.logger.Warn("diagnostic: failed to restore tokens, clearing instead", "error", err)
						_ = clearExpiredTokens()
					}
				} else {
					m.logger.Warn("diagnostic: auth error detected, clearing token (no durable token to restore)",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := clearExpiredTokens(); err != nil {
						m.logger.Warn("diagnostic: failed to clear tokens", "error", err)
					}
				}
			} else if paneShowsCLIReady(strings.Split(output, "\n")) {
				m.logger.Info("diagnostic: copilot started successfully in bare mode", "agent", agent.Name)
				agent.LastError = ""
			} else {
				continue
			}

			if agent.UID > 0 {
				killAgentProcesses(agent.UID, m.logger)
			}
			_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
			agent.forceRelaunch = true
			if err := m.Restart(ctx, agent.Name); err != nil {
				m.logger.Warn("diagnostic: restart failed", "agent", agent.Name, "error", err)
			}
			return
		}
	}
}

// fixSharedConfigPerms ensures /data/home/.copilot/config.json is group-readable
// before launching an agent. Copilot CLI rewrites this file with 600 perms on
// token refresh, locking out other agent UIDs that share the same HOME.
func (m *Manager) fixSharedConfigPerms(agent *AgentProcess) {
	info, err := os.Stat(sharedCopilotConfigPath)
	if err != nil {
		return
	}
	if info.Mode().Perm() == sharedConfigDesiredMode {
		return
	}
	m.logger.Warn("fixing shared config.json perms before launch",
		"agent", agent.Name,
		"was", fmt.Sprintf("%04o", info.Mode().Perm()),
		"fix", fmt.Sprintf("%04o", sharedConfigDesiredMode))
	if err := os.Chmod(sharedCopilotConfigPath, sharedConfigDesiredMode); err != nil {
		m.logger.Warn("failed to fix config.json perms", "error", err)
	}
}
