package config

import "testing"

func TestCadenceOwnershipClaimAndRelease(t *testing.T) {
	var g GovernorConfig
	if g.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatal("zero-value governor must report no operator ownership")
	}
	g.ClaimCadenceOwnership("surge", "scanner")
	if !g.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatal("claim did not stick")
	}
	if g.CadenceIsOperatorOwned("busy", "scanner") || g.CadenceIsOperatorOwned("surge", "guide") {
		t.Fatal("claim leaked to a different mode/agent")
	}
	g.ReleaseCadenceOwnership("surge", "scanner")
	if g.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatal("release did not clear the claim")
	}
	if len(g.CadenceOwners) != 0 {
		t.Fatalf("release left an empty mode map behind: %+v", g.CadenceOwners)
	}
	// Releasing something never claimed must be a no-op, not a panic.
	g.ReleaseCadenceOwnership("idle", "quality")
}

func TestAdoptOperatorCadenceOverrides(t *testing.T) {
	seed := &Config{Governor: GovernorConfig{
		Modes: map[string]ModeConfig{
			"surge": {Cadences: map[string]Cadence{"scanner": "4h", "guide": "4h"}},
		},
	}}
	overlay := &Config{Governor: GovernorConfig{
		Modes: map[string]ModeConfig{
			"surge": {Cadences: map[string]Cadence{"scanner": "9h", "guide": "1m"}},
			"idle":  {Cadences: map[string]Cadence{"quality": "12h"}},
		},
		CadenceOwners: map[string]map[string]string{
			"surge": {"scanner": FieldOwnerOperator, "guide": FieldOwnerPack},
			"idle":  {"quality": FieldOwnerOperator},
			// Marker without a matching cadence value: must be skipped.
			"busy": {"scanner": FieldOwnerOperator},
		},
	}}

	adoptOperatorCadenceOverrides(seed, overlay)

	if got := string(seed.Governor.Modes["surge"].Cadences["scanner"]); got != "9h" {
		t.Errorf("operator-owned cadence not adopted: surge/scanner = %q, want 9h", got)
	}
	if !seed.Governor.CadenceIsOperatorOwned("surge", "scanner") {
		t.Error("ownership marker not adopted with the value")
	}
	if got := string(seed.Governor.Modes["surge"].Cadences["guide"]); got != "4h" {
		t.Errorf("pack-owned overlay cadence must not override the seed: surge/guide = %q, want 4h", got)
	}
	if got := string(seed.Governor.Modes["idle"].Cadences["quality"]); got != "12h" {
		t.Errorf("operator-owned cadence in a mode the seed lacks not adopted: idle/quality = %q, want 12h", got)
	}
	if _, ok := seed.Governor.Modes["busy"]; ok {
		t.Error("a dangling ownership marker must not materialize a mode")
	}
}
