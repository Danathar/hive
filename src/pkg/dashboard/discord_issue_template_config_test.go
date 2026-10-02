package dashboard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiscordIssueTemplateConfigWellFormed(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/ISSUE_TEMPLATE/config.yml")
	if err != nil {
		t.Fatalf("reading issue template config: %v", err)
	}
	var cfg struct {
		BlankIssuesEnabled bool `yaml:"blank_issues_enabled"`
		ContactLinks       []struct {
			Name  string `yaml:"name"`
			URL   string `yaml:"url"`
			About string `yaml:"about"`
		} `yaml:"contact_links"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("issue template config is not valid YAML: %v", err)
	}
	found := false
	for _, link := range cfg.ContactLinks {
		if link.Name == "Join our Discord" {
			found = true
			if link.URL != "https://hivecommons.dev/discord" || !strings.Contains(link.About, "Hive Commons Discord") {
				t.Fatalf("Discord contact link = %#v", link)
			}
		}
	}
	if !found {
		t.Fatal("issue template config missing Join our Discord contact link")
	}
}
