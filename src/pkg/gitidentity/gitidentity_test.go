package gitidentity

import "testing"

func TestAgentIdentityUsesDefaultDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")

	name, email, ok := AgentIdentity("quality-agent")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for a safe agent name")
	}
	if name != "quality-agent" {
		t.Fatalf("name = %q, want quality-agent", name)
	}
	if email != "quality-agent@"+DefaultBotEmailDomain {
		t.Fatalf("email = %q, want default domain", email)
	}
}

func TestAgentIdentityHonorsValidDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example")

	_, email, ok := AgentIdentity("release_5")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for underscore-safe agent name")
	}
	if email != "release_5@bots.example" {
		t.Fatalf("email = %q, want release_5@bots.example", email)
	}
}

func TestAgentIdentityRejectsUnsafeAgentName(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example")

	if name, email, ok := AgentIdentity("bad/name"); ok || name != "" || email != "" {
		t.Fatalf("AgentIdentity unsafe name = (%q, %q, %v), want empty false", name, email, ok)
	}
}

func TestAgentIdentityFallsBackFromUnsafeDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bad/domain")

	_, email, ok := AgentIdentity("worker")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for safe agent name")
	}
	if email != "worker@"+DefaultBotEmailDomain {
		t.Fatalf("email = %q, want default domain", email)
	}
}
