package config

import "testing"

// TestCuratorDisabledByDefault pins the opt-in contract for #5430 at the config
// layer: a knowledge block that says nothing about the curator must report
// disabled, so the promotion scheduler never starts on an existing hive that is
// merely upgraded.
func TestCuratorDisabledByDefault(t *testing.T) {
	cfg := &Config{}
	cfg.Knowledge.Enabled = true
	cfg.applyDefaults()

	if cfg.Knowledge.Curator.IsEnabled() {
		t.Fatal("curator must be disabled when knowledge.curator.enabled is absent")
	}
	// The pre-#5430 code stamped "daily" here unconditionally. Leaving it empty
	// on a disabled hive keeps the effective config honest about what runs.
	if cfg.Knowledge.Curator.Schedule != "" {
		t.Errorf("schedule defaulted to %q on a hive that never opted in; want empty",
			cfg.Knowledge.Curator.Schedule)
	}
}

// TestCuratorExplicitFalseStaysDisabled covers the third state the pointer
// makes representable — set, and set to off.
func TestCuratorExplicitFalseStaysDisabled(t *testing.T) {
	off := false
	cfg := &Config{}
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.Curator.Enabled = &off
	cfg.Knowledge.Curator.Schedule = "hourly"
	cfg.applyDefaults()

	if cfg.Knowledge.Curator.IsEnabled() {
		t.Error("enabled:false must stay disabled")
	}
	// An explicitly configured schedule is preserved verbatim; it simply is not
	// acted on while disabled.
	if cfg.Knowledge.Curator.Schedule != "hourly" {
		t.Errorf("schedule = %q, want it preserved as \"hourly\"", cfg.Knowledge.Curator.Schedule)
	}
}

// TestCuratorEnabledGetsScheduleDefault shows the default cadence is still
// applied — but only once an operator has opted in.
func TestCuratorEnabledGetsScheduleDefault(t *testing.T) {
	on := true
	cfg := &Config{}
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.Curator.Enabled = &on
	cfg.applyDefaults()

	if !cfg.Knowledge.Curator.IsEnabled() {
		t.Fatal("enabled:true must report enabled")
	}
	if cfg.Knowledge.Curator.Schedule != defaultCuratorSchedule {
		t.Errorf("schedule = %q, want %q", cfg.Knowledge.Curator.Schedule, defaultCuratorSchedule)
	}
	if cfg.Knowledge.Curator.AutoPromoteThreshold != defaultPromoteThreshold {
		t.Errorf("threshold = %v, want %v",
			cfg.Knowledge.Curator.AutoPromoteThreshold, defaultPromoteThreshold)
	}
}

// TestCuratorEnabledPreservesExplicitSchedule ensures the default never
// overwrites an operator's choice.
func TestCuratorEnabledPreservesExplicitSchedule(t *testing.T) {
	on := true
	cfg := &Config{}
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.Curator.Enabled = &on
	cfg.Knowledge.Curator.Schedule = "hourly"
	cfg.applyDefaults()

	if cfg.Knowledge.Curator.Schedule != "hourly" {
		t.Errorf("schedule = %q, want \"hourly\"", cfg.Knowledge.Curator.Schedule)
	}
}
