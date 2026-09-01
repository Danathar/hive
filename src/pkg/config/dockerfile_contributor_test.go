package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContributorDockerfileInstallsPiWithoutCurlPipeShell(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	dockerfile := string(body)
	if strings.Contains(dockerfile, "https://pi.dev/install.sh | sh") {
		t.Fatal("Dockerfile.contributor must not execute the mutable pi.dev installer with curl|sh")
	}
	for _, want := range []string{
		"ARG PI_CODING_AGENT_VERSION=0.84.1",
		"@earendil-works/pi-coding-agent@${PI_CODING_AGENT_VERSION}",
		"npm install -g --ignore-scripts",
		"which pi",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q", want)
		}
	}
}

// The image installs claude-code with --ignore-scripts (deliberately — no
// arbitrary postinstall runs during the build), but that also skips the
// package's install.cjs, which links the platform-native binary into bin/.
// Without an explicit postinstall + verification, `claude` in the container
// dies with "claude native binary not installed" on every task while local
// mode works fine. src/Dockerfile Layer 7 fixed this for the spoke image;
// this pins the contributor image's copy of the same fix.
func TestContributorDockerfileLinksClaudeNativeBinary(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	dockerfile := string(body)
	for _, want := range []string{
		`node "$(npm root -g)/@anthropic-ai/claude-code/install.cjs"`,
		"claude --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — claude's native binary is not linked (or not verified) at build time, so container-mode claude fails at runtime with 'claude native binary not installed'", want)
		}
	}
}
