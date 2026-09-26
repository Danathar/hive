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
// ownership drift #4596 fixed. With jev_mode off nothing is written.
func (m *Manager) installJevForAgent(agent *AgentProcess, backend string) {
	if !agent.Config.JevEnabled() {
		return
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}
	dir := jevSkillDir(home, codexHomePath(agent.Name), backend)
	if dir == "" {
		m.logger.Info("jev skill not supported for backend; agent keeps HIVE_JEV_MODE and can still run `hive jev decide`",
			"agent", agent.Name, "backend", backend)
		return
	}
	userSpec := ""
	if agent.UID > 0 {
		userSpec = m.agentExecUserSpec(agent)
	}
	if err := installJevSkill(dir, userSpec); err != nil {
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
