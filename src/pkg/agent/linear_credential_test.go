package agent

import (
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func linearCredManager(t *testing.T, mode string, cred LinearCredential) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: mode},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	m.SetLinearCredentialResolver(func() LinearCredential { return cred })
	m.mu.RLock()
	a := m.agents["a"]
	m.mu.RUnlock()
	return m, a
}

func envByKey(m *Manager, a *AgentProcess) map[string]agentEnvPair {
	out := map[string]agentEnvPair{}
	for _, p := range m.agentEnvPairs(a) {
		out[p.Key] = p
	}
	return out
}

// An ISSUES_ONLY agent — the tier at which the proxy first permits a Linear
// mutation — gets the OAuth token, as a secret, under the Bearer-form name.
func TestLinearCredential_IssuesOnlyGetsAccessToken(t *testing.T) {
	m, a := linearCredManager(t, "ISSUES_ONLY", LinearCredential{AccessToken: "lin_oauth_x"})
	env := envByKey(m, a)
	p, ok := env["LINEAR_ACCESS_TOKEN"]
	if !ok || p.Value != "lin_oauth_x" || !p.Secret {
		t.Fatalf("LINEAR_ACCESS_TOKEN = %+v (present=%v), want secret lin_oauth_x", p, ok)
	}
	if _, ok := env["LINEAR_API_KEY"]; ok {
		t.Error("LINEAR_API_KEY must not be set when the OAuth token is available")
	}
}

// Without a connected workspace the work-source API key is the fallback.
func TestLinearCredential_APIKeyFallback(t *testing.T) {
	m, a := linearCredManager(t, "ISSUES_AND_PRS", LinearCredential{APIKey: "lin_api_y"})
	env := envByKey(m, a)
	p, ok := env["LINEAR_API_KEY"]
	if !ok || p.Value != "lin_api_y" || !p.Secret {
		t.Fatalf("LINEAR_API_KEY = %+v (present=%v), want secret lin_api_y", p, ok)
	}
	if _, ok := env["LINEAR_ACCESS_TOKEN"]; ok {
		t.Error("LINEAR_ACCESS_TOKEN must not be set without an OAuth token")
	}
}

// Advisory agents stay credential-less (GitHub parity: no GITHUB_TOKEN below
// the push tier), even when a credential is configured.
func TestLinearCredential_AdvisoryGetsNothing(t *testing.T) {
	m, a := linearCredManager(t, "ADVISORY", LinearCredential{AccessToken: "x", APIKey: "y"})
	env := envByKey(m, a)
	for _, k := range []string{"LINEAR_ACCESS_TOKEN", "LINEAR_API_KEY"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be injected into an ADVISORY agent", k)
		}
	}
}

// No resolver (tests / GitHub-only hives) injects nothing and never panics.
func TestLinearCredential_NoResolver(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	a := m.agents["a"]
	m.mu.RUnlock()
	env := envByKey(m, a)
	for _, k := range []string{"LINEAR_ACCESS_TOKEN", "LINEAR_API_KEY"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s injected with no resolver", k)
		}
	}
	m.SetLinearCredentialResolver(nil)
	if !m.linearCredential().Empty() {
		t.Error("nil resolver should yield an empty credential")
	}
}
