package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sentinel.clear_counts_as_approval is off unless the operator writes it,
// reads back from YAML, and is left out of a saved config while off.
func TestSentinelClearCountsAsApprovalDefaultsOff(t *testing.T) {
	var unset SentinelConfig
	if err := yaml.Unmarshal([]byte("trusted_authors_block: true\n"), &unset); err != nil {
		t.Fatal(err)
	}
	if unset.ClearCountsAsApproval {
		t.Fatal("clear_counts_as_approval must default to off")
	}
	out, err := yaml.Marshal(unset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "clear_counts_as_approval") {
		t.Fatalf("an off setting must not be written out:\n%s", out)
	}

	var on SentinelConfig
	if err := yaml.Unmarshal([]byte("clear_counts_as_approval: true\n"), &on); err != nil {
		t.Fatal(err)
	}
	if !on.ClearCountsAsApproval {
		t.Fatal("clear_counts_as_approval: true not read")
	}
	if err := ValidateSentinel(on); err != nil {
		t.Fatalf("a config with the setting on must validate: %v", err)
	}
	out, err = yaml.Marshal(on)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "clear_counts_as_approval: true") {
		t.Fatalf("an on setting must round-trip:\n%s", out)
	}
}
