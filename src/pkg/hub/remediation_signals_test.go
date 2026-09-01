package hub

import (
	"fmt"
	"net/http"
	"testing"
)

// Remediation-hint detector fields (#5577): AgentErrorStreaks, ConsentWedged,
// NoCadenceAgents must round-trip handleHeartbeat sanitized, carry forward
// across minimal beats that omit them, and CLEAR when a beat carries a
// measured empty value — the nil-vs-empty distinction the whole design rests
// on (a recovered hive must be able to clear its own alarm).

// storedEntry fetches a registry entry by hive id.
func storedEntry(t *testing.T, s *HubServer, id string) RegistryEntry {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return RegistryEntry{}
}

func TestHeartbeatRemediationSignals_RoundTrip(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{
		"hive_id":"h1",
		"agent_error_streaks":{"quality":17},
		"consent_wedged":["copilot-fixer"],
		"no_cadence_agents":["telemetry","docs"]
	}`
	if rec := postHeartbeat(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	e := storedEntry(t, s, "h1")
	if e.AgentErrorStreaks == nil || e.AgentErrorStreaks["quality"] != 17 {
		t.Errorf("AgentErrorStreaks = %v, want quality:17", e.AgentErrorStreaks)
	}
	if len(e.ConsentWedged) != 1 || e.ConsentWedged[0] != "copilot-fixer" {
		t.Errorf("ConsentWedged = %v, want [copilot-fixer]", e.ConsentWedged)
	}
	if len(e.NoCadenceAgents) != 2 || e.NoCadenceAgents[0] != "telemetry" || e.NoCadenceAgents[1] != "docs" {
		t.Errorf("NoCadenceAgents = %v, want [telemetry docs]", e.NoCadenceAgents)
	}
}

// A minimal beat (spoke restarting, collectors not warm) omits the fields —
// nil on decode — and must NOT blank the last real measurement; a later beat
// carrying a measured EMPTY value must clear it.
func TestHeartbeatRemediationSignals_CarryForwardAndClear(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	full := `{
		"hive_id":"h1",
		"agent_error_streaks":{"quality":5},
		"consent_wedged":["copilot-fixer"],
		"no_cadence_agents":["telemetry"]
	}`
	if rec := postHeartbeat(t, s, full); rec.Code != http.StatusOK {
		t.Fatalf("full beat status = %d", rec.Code)
	}

	// Minimal beat: none of the three keys present.
	if rec := postHeartbeat(t, s, `{"hive_id":"h1"}`); rec.Code != http.StatusOK {
		t.Fatalf("minimal beat status = %d", rec.Code)
	}
	e := storedEntry(t, s, "h1")
	if e.AgentErrorStreaks == nil || e.AgentErrorStreaks["quality"] != 5 {
		t.Errorf("carry-forward dropped AgentErrorStreaks: %v", e.AgentErrorStreaks)
	}
	if len(e.ConsentWedged) != 1 {
		t.Errorf("carry-forward dropped ConsentWedged: %v", e.ConsentWedged)
	}
	if len(e.NoCadenceAgents) != 1 {
		t.Errorf("carry-forward dropped NoCadenceAgents: %v", e.NoCadenceAgents)
	}

	// Measured all-clear: explicit empty values must OVERWRITE — this is how
	// a fixed hive clears its own alarm.
	clearBody := `{
		"hive_id":"h1",
		"agent_error_streaks":{},
		"consent_wedged":[],
		"no_cadence_agents":[]
	}`
	if rec := postHeartbeat(t, s, clearBody); rec.Code != http.StatusOK {
		t.Fatalf("clear beat status = %d", rec.Code)
	}
	e = storedEntry(t, s, "h1")
	if len(e.AgentErrorStreaks) != 0 || e.AgentErrorStreaks == nil {
		t.Errorf("measured empty AgentErrorStreaks did not clear/stay measured: %v", e.AgentErrorStreaks)
	}
	if len(e.ConsentWedged) != 0 || e.ConsentWedged == nil {
		t.Errorf("measured empty ConsentWedged did not clear/stay measured: %v", e.ConsentWedged)
	}
	if len(e.NoCadenceAgents) != 0 || e.NoCadenceAgents == nil {
		t.Errorf("measured empty NoCadenceAgents did not clear/stay measured: %v", e.NoCadenceAgents)
	}
}

// Hostile/broken payloads: names are identifier-sanitized, non-positive
// streaks dropped, values clamped, and collection sizes capped at the same
// bound as the agent list itself.
func TestHeartbeatRemediationSignals_SanitizeAndClamp(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{
		"hive_id":"h1",
		"agent_error_streaks":{"qua<script>lity":9,"neg":-4,"huge":999999},
		"consent_wedged":["<b>x</b>",""],
		"no_cadence_agents":["ok-agent","<>&"]
	}`
	if rec := postHeartbeat(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}

	e := storedEntry(t, s, "h1")
	if v := e.AgentErrorStreaks["quascriptlity"]; v != 9 {
		t.Errorf("sanitized streak key missing/wrong: %v", e.AgentErrorStreaks)
	}
	if _, ok := e.AgentErrorStreaks["neg"]; ok {
		t.Errorf("negative streak kept: %v", e.AgentErrorStreaks)
	}
	if v := e.AgentErrorStreaks["huge"]; v != maxAgentErrorStreak {
		t.Errorf("streak not clamped: got %d want %d", v, maxAgentErrorStreak)
	}
	// "<b>x</b>" sanitizes to "bx/b" (angle brackets stripped) and "" drops.
	if len(e.ConsentWedged) != 1 || e.ConsentWedged[0] != "bx/b" {
		t.Errorf("ConsentWedged sanitize wrong: %v", e.ConsentWedged)
	}
	// "<>&" sanitizes to empty and drops; the list stays measured (non-nil).
	if len(e.NoCadenceAgents) != 1 || e.NoCadenceAgents[0] != "ok-agent" {
		t.Errorf("NoCadenceAgents sanitize wrong: %v", e.NoCadenceAgents)
	}
}

func TestSanitizeRemediationSignals_Bounds(t *testing.T) {
	// nil stays nil (not measured), never an empty measurement.
	if sanitizeAgentErrorStreaks(nil) != nil {
		t.Error("nil streak map must stay nil")
	}
	if sanitizeAgentNameList(nil) != nil {
		t.Error("nil name list must stay nil")
	}

	// Oversized collections are capped at maxRemediationAgents.
	bigMap := make(map[string]int, maxRemediationAgents+10)
	var bigList []string
	for i := 0; i < maxRemediationAgents+10; i++ {
		name := fmt.Sprintf("agent-%03d", i)
		bigMap[name] = 1
		bigList = append(bigList, name)
	}
	if got := sanitizeAgentErrorStreaks(bigMap); len(got) != maxRemediationAgents {
		t.Errorf("streak map not capped: %d", len(got))
	}
	if got := sanitizeAgentNameList(bigList); len(got) != maxRemediationAgents {
		t.Errorf("name list not capped: %d", len(got))
	}
}
