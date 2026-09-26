package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/jev"
)

// installJevForAgent seeds the jev-decide skill into the agent's CLI home
// before launch when jev_mode is assist (hivecommons/hive#8939). The caveman
// analogue (installCavemanForAgent) runs an external installer; Jev's skill
// is a single embedded SKILL.md, so it is written directly — AS the agent
// user, for the same reason setupCodexHome does: the manager runs as dev and
// cannot chown, and a dev-owned file under a per-UID home is exactly the
// ownership drift #4596 fixed. With jev_mode off nothing is written, and a
// SKILL.md left by an earlier assist run is removed so the agent stops
// advertising a tool that would now only be refused.
func (m *Manager) installJevForAgent(agent *AgentProcess, backend string) {
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}
	dir := jevSkillDir(home, codexHomePath(agent.Name), backend)
	if !agent.Config.JevEnabled() {
		if dir == "" {
			return
		}
		if _, err := os.Lstat(dir); err != nil {
			return // never installed (or home not yet created): nothing to undo
		}
		if err := removeJevSkill(dir, m.agentExecUserSpec(agent)); err != nil {
			m.logger.Warn("jev skill removal failed; agent keeps a stale skill file", "agent", agent.Name, "backend", backend, "dir", dir, "error", err)
			return
		}
		m.logger.Info("removed jev skill (jev_mode off)", "agent", agent.Name, "backend", backend, "dir", dir)
		return
	}
	if dir == "" {
		m.logger.Info("jev skill not supported for backend; agent keeps HIVE_JEV_MODE and can still run `hive jev decide`",
			"agent", agent.Name, "backend", backend)
		return
	}
	if err := installJevSkill(dir, m.agentExecUserSpec(agent)); err != nil {
		m.logger.Warn("jev skill install failed", "agent", agent.Name, "backend", backend, "dir", dir, "error", err)
		return
	}
	m.logger.Info("installed jev skill", "agent", agent.Name, "backend", backend, "dir", dir)
}

// jevSkillDir resolves where backend reads skills from: under CODEX_HOME for
// codex (the manager exports a per-agent one), under HOME for the rest. Empty
// when the backend has no known skills directory.
func jevSkillDir(home, codexHome, backend string) string {
	rel := jev.SkillRelDir(backend)
	if rel == "" {
		return ""
	}
	if backend == codexBackend {
		return filepath.Join(codexHome, filepath.FromSlash(rel))
	}
	return filepath.Join(home, filepath.FromSlash(rel))
}

// installJevSkill writes SKILL.md into dir, creating it. A non-empty userSpec
// runs both steps through su-exec as that user.
func installJevSkill(dir, userSpec string) error {
	path := filepath.Join(dir, jev.SkillFile)
	if userSpec == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, jev.SkillMarkdown(), 0o644)
	}
	if out, err := exec.Command("su-exec", userSpec, "mkdir", "-p", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir %s as %s: %w: %s", dir, userSpec, err, string(out))
	}
	return writeFileAsUser(userSpec, path, jev.SkillMarkdown())
}

// removeJevSkill deletes the jev-decide skill directory (ours in full: it
// holds only SKILL.md). A non-empty userSpec removes it through su-exec as
// that user, mirroring installJevSkill.
func removeJevSkill(dir, userSpec string) error {
	if userSpec == "" {
		return os.RemoveAll(dir)
	}
	if out, err := exec.Command("su-exec", userSpec, "rm", "-rf", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("rm -rf %s as %s: %w: %s", dir, userSpec, err, string(out))
	}
	return nil
}
