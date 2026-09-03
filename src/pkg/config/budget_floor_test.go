package config

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// budgetYAML is minimalValidYAML plus an explicit governor budget limit.
func budgetYAML(totalTokens string) string {
	return minimalValidYAML("my-org", "ghp_test") + `
governor:
  budget:
    total_tokens: ` + totalTokens + `
`
}

// captureLog redirects the standard logger for the duration of fn and returns
// everything written. The load-path warning is a log line, so the only honest
// way to assert it fired is to read the log.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	origOut, origFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	fn()
	return buf.String()
}

// TestBelowFloorBudgetStillLoads is the load-path half of the #5508 asymmetry
// and the guard against a future "make validation consistent" refactor.
//
// Three spokes are live with limits of 5, 50 and 1000 tokens. If config load
// ever REJECTS a below-floor limit, those spokes stop booting — a starving
// hive becomes a dead one. Load must WARN and carry the value through
// UNCHANGED. Its twin is TestBelowFloorBudgetSaveRejected in pkg/dashboard,
// which asserts the OPPOSITE behaviour for the same values on the save path.
func TestBelowFloorBudgetStillLoads(t *testing.T) {
	// The exact limits found on the live fleet by the #5508 audit.
	for _, tc := range []struct {
		name  string
		yaml  string
		limit int64
	}{
		{"devx-gabriel 5 tokens", "5", 5},
		{"z-aiops2 50 tokens", "50", 50},
		{"hosted qa-test 1000 tokens", "1000", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, budgetYAML(tc.yaml))

			var cfg *Config
			var err error
			out := captureLog(t, func() { cfg, err = Load(path) })

			// The load MUST succeed. This is the assertion that keeps the
			// three live spokes bootable.
			if err != nil {
				t.Fatalf("Load() rejected a below-floor budget (%d tokens): %v; "+
					"load must WARN, never reject — rejecting stops live spokes from starting",
					tc.limit, err)
			}
			// And it must carry the operator's value through untouched: silently
			// bumping it to the floor would spend real tokens they never authorized.
			if cfg.Governor.Budget.TotalTokens != tc.limit {
				t.Errorf("Load() changed total_tokens to %d, want %d left unchanged",
					cfg.Governor.Budget.TotalTokens, tc.limit)
			}
			// The warning is the entire user-visible value of the load path.
			if !strings.Contains(out, "WARNING") ||
				!strings.Contains(out, "governor.budget.total_tokens") {
				t.Errorf("no prominent warning logged for a %d-token budget; log was:\n%s", tc.limit, out)
			}
			// It must name the likely unit mistake, which is the whole point.
			if !strings.Contains(out, "did you mean") {
				t.Errorf("warning omits the unit-mistake hint for %d tokens; log was:\n%s", tc.limit, out)
			}
		})
	}
}

// TestSaneBudgetLoadsWithoutWarning honours the issue's "no behavior change for
// sane configs" literally: at or above the floor, nothing is logged and nothing
// is altered.
func TestSaneBudgetLoadsWithoutWarning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		limit int64
	}{
		{"exactly at the floor", "100000", MinUsableBudgetTokens},
		{"a realistic 50M budget", "50000000", 50_000_000},
		// Zero is the documented way to disable budget tracking. It is below
		// the floor numerically and must NOT be warned about.
		{"zero disables budget tracking", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, budgetYAML(tc.yaml))

			var cfg *Config
			var err error
			out := captureLog(t, func() { cfg, err = Load(path) })

			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Governor.Budget.TotalTokens != tc.limit {
				t.Errorf("total_tokens = %d, want %d", cfg.Governor.Budget.TotalTokens, tc.limit)
			}
			if strings.Contains(out, "total_tokens") {
				t.Errorf("sane budget (%d) produced a budget warning; log was:\n%s", tc.limit, out)
			}
		})
	}
}

// TestBudgetLimitBelowFloor pins the predicate both paths share, including the
// zero exemption that keeps "budget tracking off" working.
func TestBudgetLimitBelowFloor(t *testing.T) {
	for _, tc := range []struct {
		limit int64
		want  bool
	}{
		{0, false}, // disabled, not a mistake
		{-1, false},
		{1, true},
		{5, true},
		{50, true},
		{1000, true},
		{MinUsableBudgetTokens - 1, true},
		{MinUsableBudgetTokens, false}, // the floor itself is acceptable
		{MinUsableBudgetTokens + 1, false},
		{50_000_000, false},
	} {
		if got := BudgetLimitBelowFloor(tc.limit); got != tc.want {
			t.Errorf("BudgetLimitBelowFloor(%d) = %v, want %v", tc.limit, got, tc.want)
		}
		// SuggestBudgetUnitMistake must agree with the predicate exactly, since
		// callers use a non-empty string as the test.
		if gotMsg := SuggestBudgetUnitMistake(tc.limit) != ""; gotMsg != tc.want {
			t.Errorf("SuggestBudgetUnitMistake(%d) non-empty = %v, want %v", tc.limit, gotMsg, tc.want)
		}
	}

	// The hint must name the value AND the megatoken reading of it — "did you
	// mean 50M?" is what makes the message actionable.
	msg := SuggestBudgetUnitMistake(50)
	if !strings.Contains(msg, "50 tokens") || !strings.Contains(msg, "50M") {
		t.Errorf("SuggestBudgetUnitMistake(50) = %q, want it to name both 50 tokens and 50M", msg)
	}
}
