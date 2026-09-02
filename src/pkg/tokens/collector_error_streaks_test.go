package tokens

import "testing"

// The heartbeat forwards nil as "not measured", never as "no failures" — so
// before any bob-sessions scan has completed the accessor must return nil,
// not an empty map.
func TestAgentErrorStreaks_NilBeforeScan(t *testing.T) {
	c := &Collector{}
	if got := c.AgentErrorStreaks(); got != nil {
		t.Errorf("AgentErrorStreaks() = %v, want nil before any scan", got)
	}
}

// After a scan, an EMPTY map means "measured, no streaks" and must survive the
// accessor as non-nil — collapsing it to nil would make a clean fleet look
// unmeasured.
func TestAgentErrorStreaks_EmptyMeansMeasured(t *testing.T) {
	c := &Collector{errorStreaks: map[string]int{}}
	got := c.AgentErrorStreaks()
	if got == nil {
		t.Fatal("AgentErrorStreaks() = nil, want non-nil empty map for a measured clean fleet")
	}
	if len(got) != 0 {
		t.Errorf("AgentErrorStreaks() = %v, want empty", got)
	}
}

// The accessor documents that it returns a copy: a caller mutating the result
// must not corrupt the collector's internal state under other readers.
func TestAgentErrorStreaks_ReturnsCopy(t *testing.T) {
	c := &Collector{errorStreaks: map[string]int{"scanner": 3}}

	got := c.AgentErrorStreaks()
	if got["scanner"] != 3 {
		t.Fatalf("AgentErrorStreaks()[scanner] = %d, want 3", got["scanner"])
	}

	got["scanner"] = 99
	got["reviewer"] = 1

	again := c.AgentErrorStreaks()
	if again["scanner"] != 3 || len(again) != 1 {
		t.Errorf("internal state mutated through returned map: %v, want map[scanner:3]", again)
	}
}
