package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/jev"
)

func jevEnv(t *testing.T, mode string) map[string]string {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude", Model: "opus", JevMode: mode}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	out := map[string]string{}
	for _, p := range m.agentEnvPairs(agent) {
		out[p.Key] = p.Value
	}
	return out
}

// TestAgentEnvPairs_JevOnlyWhenAssist: HIVE_JEV_MODE / HIVE_JEV_ENDPOINT are
// exported only for jev_mode: assist — "" and "off" export nothing, so a
// default agent's environment is byte-identical to before the feature.
func TestAgentEnvPairs_JevOnlyWhenAssist(t *testing.T) {
	for _, mode := range []string{"", config.JevModeOff} {
		env := jevEnv(t, mode)
		if _, has := env[jev.ModeEnvVar]; has {
			t.Errorf("jev_mode=%q must not export %s", mode, jev.ModeEnvVar)
		}
		if _, has := env[jev.EndpointEnvVar]; has {
			t.Errorf("jev_mode=%q must not export %s", mode, jev.EndpointEnvVar)
		}
	}
	env := jevEnv(t, config.JevModeAssist)
	if env[jev.ModeEnvVar] != "assist" || env[jev.EndpointEnvVar] != jev.DefaultEndpoint {
		t.Errorf("assist env = %q / %q", env[jev.ModeEnvVar], env[jev.EndpointEnvVar])
	}
	for k, v := range env {
		if strings.Contains(strings.ToUpper(k), "JEV") && strings.Contains(k, "KEY") {
			t.Errorf("no Jev key may be exported to an agent: %s=%q", k, v)
		}
	}
}

func TestJevSkillDir(t *testing.T) {
	cases := map[string]string{
		"claude":  "/home/a/.claude/skills/jev-decide",
		"codex":   "/data/home/.codex-a/skills/jev-decide",
		"copilot": "/home/a/.copilot/skills/jev-decide",
		"gemini":  "/home/a/.gemini/skills/jev-decide",
		"goose":   "/home/a/.config/goose/skills/jev-decide",
		"aider":   "",
	}
	for backend, want := range cases {
		if got := jevSkillDir("/home/a", "/data/home/.codex-a", backend); got != want {
			t.Errorf("%s: %q, want %q", backend, got, want)
		}
	}
}

// TestInstallJevSkill_WritesEmbeddedSkill: the direct (shared-UID) path lays
// down SKILL.md verbatim and is idempotent.
func TestInstallJevSkill_WritesEmbeddedSkill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills", "jev-decide")
	for i := range 2 {
		if err := installJevSkill(dir, ""); err != nil {
			t.Fatalf("install #%d: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, jev.SkillFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(jev.SkillMarkdown()) {
		t.Fatal("installed SKILL.md differs from the embedded skill")
	}
}

// TestInstallJevForAgent_OffInstallsNothing: the default agent gets no skill
// file — the caller does not even resolve a home.
func TestInstallJevForAgent_OffInstallsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	m.installJevForAgent(agent, "claude")
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "jev-decide", jev.SkillFile)); !os.IsNotExist(err) {
		t.Fatalf("jev_mode off must install nothing (stat err=%v)", err)
	}
}

// TestInstallJevForAgent_AssistSharedUIDWritesToHome: with UID 0 (shared dev
// UID) the skill lands under $HOME for the backend's skills dir.
func TestInstallJevForAgent_AssistSharedUIDWritesToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "gemini", JevMode: config.JevModeAssist}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	agent.UID = 0
	m.installJevForAgent(agent, "gemini")
	if _, err := os.Stat(filepath.Join(home, ".gemini", "skills", "jev-decide", jev.SkillFile)); err != nil {
		t.Fatalf("expected skill under HOME: %v", err)
	}
	// Unsupported backend: logged and skipped, nothing written anywhere under HOME.
	m.installJevForAgent(agent, "aider")
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != ".gemini" {
			t.Errorf("unsupported backend wrote %s", e.Name())
		}
	}
}

// TestInstallJevForAgent_OffRemovesEarlierSkill: an agent that ran with
// assist and is restarted with jev_mode off loses the SKILL.md (the whole
// jev-decide dir), so it stops advertising a tool the hive now refuses.
// Sibling skills in the same skills dir are untouched.
func TestInstallJevForAgent_OffRemovesEarlierSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude", JevMode: config.JevModeAssist}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	agent.UID = 0
	m.installJevForAgent(agent, "claude")
	skillDir := filepath.Join(home, ".claude", "skills", "jev-decide")
	if _, err := os.Stat(filepath.Join(skillDir, jev.SkillFile)); err != nil {
		t.Fatalf("assist must install the skill: %v", err)
	}
	other := filepath.Join(home, ".claude", "skills", "other", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("# other"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent.Config.JevMode = config.JevModeOff
	m.installJevForAgent(agent, "claude")
	if _, err := os.Lstat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("jev_mode off must remove the earlier skill dir (lstat err=%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("sibling skill must survive: %v", err)
	}
	// Idempotent: a second off launch with nothing to remove is a no-op.
	m.installJevForAgent(agent, "claude")
}
