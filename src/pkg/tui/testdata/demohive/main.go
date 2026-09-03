// Command demohive serves a fixed, in-memory imitation of the Hive dashboard
// API — just the handful of routes pkg/tui/client calls — so that the VHS tape
// in src/docs/design/tui.tape can record `hivectl tui` deterministically.
//
// It exists for one reason: a terminal recording is a screenshot of whatever
// was on screen, and pointing the recorder at a real hive would publish that
// hive's agent names, repositories, token spend and identity into a public
// repo. Every value below is invented.
//
// Run it with:
//
//	go run ./pkg/tui/testdata/demohive          # listens on 127.0.0.1:3001
//	go run ./pkg/tui/testdata/demohive -addr 127.0.0.1:3099
//
// Stop it with ctrl+c, or `kill` the pid — it holds no state on disk, opens no
// outbound connection, and writes nothing outside its own process memory.
//
// Determinism rules this fixture follows, because the tape depends on them:
//
//   - Every payload is a fixed literal. Nothing is sampled from a clock, a
//     random source, the environment, or the host.
//   - Timestamps are frozen at a fixed instant (see fixedNow). Relative
//     "last activity" labels in the Agents pane are therefore stable from run
//     to run rather than drifting with wall time.
//   - Writes are accepted and acknowledged but change nothing that a later
//     read returns, EXCEPT pause/resume, which flips one in-memory bit so the
//     tape can show the status glyph actually changing. Restarting the process
//     restores the starting state.
//   - The SSE stream emits one frame immediately (so the header's `ws:` field
//     reaches `connected` promptly and at a predictable moment), then repeats
//     the same frame on a fixed interval. No frame ever carries new numbers.
//
// It is not a mock framework and is not imported by any test; it is a fixture
// binary for the recording, deliberately kept to one file of stdlib Go so a
// reviewer can read the whole thing before trusting it with a demo.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultAddr matches the TUI's own default HIVE_DASHBOARD_URL
// (http://localhost:3001), so the tape needs no environment override at all.
const defaultAddr = "127.0.0.1:3001"

// fixedNow is the single instant every timestamp in this fixture derives from.
// A frozen clock is what makes the Agents pane's relative activity labels
// ("2m ago") identical on every render instead of counting up while recording.
var fixedNow = time.Date(2026, time.January, 15, 9, 30, 0, 0, time.UTC)

// sseFrameInterval is how often the stream repeats its snapshot. It is well
// under the TUI's 60s settled reconcile cadence so a recording that runs for
// a minute never watches the stream go quiet, and well over the frame rate so
// the repeats cost nothing.
const sseFrameInterval = 10 * time.Second

// paused holds the only mutable bit in the fixture: whether the demo agent the
// tape pauses is currently paused. Everything else is a constant.
var paused sync.Map // agent name -> bool

func main() {
	addr := flag.String("addr", defaultAddr, "address to listen on (loopback only)")
	flag.Parse()

	if !strings.HasPrefix(*addr, "127.0.0.1:") && !strings.HasPrefix(*addr, "localhost:") {
		log.Fatalf("demohive refuses to bind %q: loopback only", *addr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/api/hive-id", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "demo-hive"})
	})
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, agentRoster())
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, statusPayload())
	})
	mux.HandleFunc("/api/config/governor", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"general_advanced": map[string]any{"eval_interval_s": 300},
		})
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, tokensPayload())
	})
	mux.HandleFunc("/api/cost", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, costPayload())
	})
	mux.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"entries": auditEntries()})
	})
	mux.HandleFunc("/api/packs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, packs())
	})
	mux.HandleFunc("/api/events", serveSSE)

	mux.HandleFunc("/api/inference/models/", func(w http.ResponseWriter, r *http.Request) {
		backend := strings.TrimPrefix(r.URL.Path, "/api/inference/models/")
		writeJSON(w, modelCatalogue(backend))
	})
	mux.HandleFunc("/api/pause/", func(w http.ResponseWriter, r *http.Request) {
		setPaused(w, r, "/api/pause/", true)
	})
	mux.HandleFunc("/api/resume/", func(w http.ResponseWriter, r *http.Request) {
		setPaused(w, r, "/api/resume/", false)
	})
	mux.HandleFunc("/api/kick/", func(w http.ResponseWriter, r *http.Request) {
		agent := strings.TrimPrefix(r.URL.Path, "/api/kick/")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"status": "queued", "agent": agent})
	})
	mux.HandleFunc("/api/model/", func(w http.ResponseWriter, r *http.Request) {
		// Path is /api/model/{agent}/{model}. The fixture acknowledges the
		// write with the values it was given and forgets them: the tape never
		// completes a model apply, and a fixture that remembered one would
		// make two consecutive runs render differently.
		rest := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/model/"), "/", 2)
		agent, model := "", ""
		if len(rest) == 2 {
			agent, model = rest[0], rest[1]
		}
		writeJSON(w, map[string]any{"status": "ok", "agent": agent, "model": model})
	})
	mux.HandleFunc("/api/packs/level", func(w http.ResponseWriter, r *http.Request) {
		// Reachable only if someone completes the ACMM typed confirmation by
		// hand. The tape cancels out of it with `esc` and never sends this.
		writeJSON(w, map[string]any{
			"ok": true, "level": 2,
			"packAgents": []string{}, "packUpdated": []string{},
			"paused": []string{}, "resumed": []string{},
		})
	})

	log.Printf("demohive listening on http://%s (fixture data only; ctrl+c to stop)", *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// agentRoster is GET /api/agents — the fleet the Agents pane draws its rows
// from. Four invented agents with generic role names; none of these exist in
// any real hive.
func agentRoster() []map[string]any {
	return []map[string]any{
		{"name": "reviewer", "id": "reviewer", "displayName": "Reviewer", "enabled": true, "managed": true, "backend": "claude", "model": "demo-model-large"},
		{"name": "docs", "id": "docs", "displayName": "Docs", "enabled": true, "managed": true, "backend": "claude", "model": "demo-model-small"},
		{"name": "triage", "id": "triage", "displayName": "Triage", "enabled": true, "managed": true, "backend": "claude", "model": "demo-model-small"},
		{"name": "planner", "id": "planner", "displayName": "Planner", "enabled": false, "managed": true, "backend": "claude", "model": "demo-model-large"},
	}
}

// statusPayload is GET /api/status and the body of every SSE frame: the live
// governor slice plus per-agent state the Agents pane joins onto the roster.
func statusPayload() map[string]any {
	agents := make([]map[string]any, 0, 4)
	for _, a := range agentRoster() {
		name := a["name"].(string)
		enabled := a["enabled"].(bool)
		isPaused := !enabled
		if v, ok := paused.Load(name); ok {
			isPaused = v.(bool)
		}
		state := "running"
		if isPaused {
			state = "stopped"
		}
		agents = append(agents, map[string]any{
			"name": name, "enabled": enabled && !isPaused,
			"paused": isPaused, "state": state,
		})
	}
	return map[string]any{
		"timestamp": fixedNow.Format(time.RFC3339),
		"agents":    agents,
		// acmmLevel / acmmLevelConfigured are TOP-LEVEL on /api/status, not
		// inside the governor object — see client.GovernorStatus, which embeds
		// GovernorState under "governor" and keeps the ACMM fields beside it.
		"acmmLevel":           2,
		"acmmLevelConfigured": true,
		"governor": map[string]any{
			"active": true, "mode": "busy",
			// The pane's "queue depth" is issues+prs, and both are flat fields
			// on the governor object. There is no nested "queue" object on the
			// wire (client/governor.go says so explicitly).
			"issues": 7, "prs": 3,
			"thresholds": map[string]any{"quiet": 2, "busy": 6, "surge": 20},
			// nextKick is a PRE-FORMATTED server-local display string on the
			// wire ("1/2 3:04 PM MST"), not a duration and not RFC 3339.
			"nextKick": "1/15 9:35 AM UTC",
		},
	}
}

// tokensPayload is GET /api/tokens. Invented counts, chosen to be large enough
// that the pane's formatting (thousands separators, per-agent breakdown) is
// visible in the recording.
func tokensPayload() map[string]any {
	return map[string]any{
		"total_tokens": 4_812_600, "total_input": 3_120_400, "total_output": 412_200,
		"total_cache_read": 1_180_000, "total_cache_create": 100_000,
		"total_messages": 942, "session_count": 4,
		"by_agent": map[string]int64{
			"reviewer": 2_140_000, "docs": 1_020_600, "triage": 980_000, "planner": 672_000,
		},
		// by_agent_detail, NOT by_agent, is what the Tokens pane builds its
		// rows from (model.tokensMsg walks ByAgentDetail). A payload with only
		// by_agent renders "no usage recorded".
		"by_agent_detail": map[string]any{
			"reviewer": map[string]any{"input": 1_390_000, "output": 186_000, "cache_read": 520_000, "cache_create": 44_000, "messages": 412, "sessions": 2},
			"docs":     map[string]any{"input": 662_000, "output": 88_600, "cache_read": 248_000, "cache_create": 22_000, "messages": 214, "sessions": 1},
			"triage":   map[string]any{"input": 636_000, "output": 84_000, "cache_read": 238_000, "cache_create": 22_000, "messages": 196, "sessions": 1},
			"planner":  map[string]any{"input": 432_400, "output": 53_600, "cache_read": 174_000, "cache_create": 12_000, "messages": 120, "sessions": 0},
		},
		"by_model": map[string]int64{
			"demo-model-large": 2_812_000, "demo-model-small": 2_000_600,
		},
		"sessions": []any{},
	}
}

// costPayload is GET /api/cost — the optional estimate beside the counts.
func costPayload() map[string]any {
	return map[string]any{
		"estimated": map[string]any{
			"total_usd": 18.42,
			// source MUST be "estimated": CostAgentEntry.Known() compares
			// against exactly that string, and an entry it does not recognise
			// is treated as unpriced and renders no dollar figure at all.
			"by_agent": []map[string]any{
				{"name": "reviewer", "usd": 8.10, "source": "estimated"},
				{"name": "docs", "usd": 3.95, "source": "estimated"},
				{"name": "triage", "usd": 3.61, "source": "estimated"},
				{"name": "planner", "usd": 2.76, "source": "estimated"},
			},
			"unpriced_models": []string{},
		},
	}
}

// auditEntries is GET /api/audit, newest first — what the Events pane shows.
// The user column is a placeholder name, not a GitHub handle.
func auditEntries() []map[string]any {
	ts := func(minutesAgo int) string {
		return fixedNow.Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339)
	}
	return []map[string]any{
		{"ts": ts(2), "user": "operator", "action": "kick", "agent": "reviewer", "detail": "manual kick queued"},
		{"ts": ts(9), "user": "governor", "action": "eval", "detail": "mode busy (queue 7/3)"},
		{"ts": ts(14), "user": "operator", "action": "model-set", "agent": "docs", "detail": "demo-model-small"},
		{"ts": ts(23), "user": "governor", "action": "kick", "agent": "triage", "detail": "scheduled run"},
		{"ts": ts(31), "user": "operator", "action": "pause", "agent": "planner", "detail": "paused by operator"},
		{"ts": ts(46), "user": "governor", "action": "eval", "detail": "mode quiet (queue 1/0)"},
		{"ts": ts(58), "user": "governor", "action": "kick", "agent": "docs", "detail": "scheduled run"},
		{"ts": ts(70), "user": "operator", "action": "resume", "agent": "docs", "detail": "resumed by operator"},
	}
}

// packs is GET /api/packs — the ACMM level definitions the overlay lists.
// Level 2 is flagged current, which is why the tape selects a DIFFERENT level
// to reach the typed-confirmation state: selecting the level already in force
// short-circuits to "nothing to apply".
func packs() []map[string]any {
	def := func(level int, name, desc string, current bool, agents int, modes, merge string) map[string]any {
		return map[string]any{
			"level": level, "name": name, "description": desc,
			"agentCount": agents, "current": current,
			"governor": map[string]any{
				"modes": modes, "mergePolicy": merge, "evalIntervalS": 300,
			},
			"agents": []any{},
		}
	}
	// Names carry NO "Ln" prefix: the overlay already renders the level, so a
	// prefixed name reads as "L3 L3 Assist".
	return []map[string]any{
		def(1, "Observe", "Read-only observation; no writes.", false, 2, "observe", "none"),
		def(2, "Advise", "Advisory findings; humans merge.", true, 4, "advisory", "manual"),
		def(3, "Assist", "Agents open PRs; humans approve.", false, 6, "advisory, active", "approval"),
		def(4, "Delegate", "Agents merge inside guardrails.", false, 8, "active", "auto-on-green"),
	}
}

// modelCatalogue is GET /api/inference/models/{backend}. It returns the list
// flagged BOTH fallback and partial on purpose: those two qualifications are
// the notes the tape is meant to show in the picker overlay, and a clean
// catalogue would render no note at all.
func modelCatalogue(backend string) map[string]any {
	return map[string]any{
		"backend":  backend,
		"fallback": true,
		"partial":  true,
		// A flat []string, NOT objects: client.ModelOption is a string type,
		// because handleInferenceModels assigns a []string regardless of what
		// openapi.json declares. Objects here decode as an error and the
		// picker renders "Catalogue unavailable" instead of a list.
		"models": []string{
			"demo-model-large",
			"demo-model-small",
			"demo-model-fast",
		},
	}
}

func setPaused(w http.ResponseWriter, r *http.Request, prefix string, want bool) {
	agent := strings.TrimPrefix(r.URL.Path, prefix)
	prev := false
	if v, ok := paused.Load(agent); ok {
		prev = v.(bool)
	}
	paused.Store(agent, want)
	state := "running"
	if want {
		state = "paused"
	}
	status := "resumed"
	if want {
		status = "paused"
	}
	writeJSON(w, map[string]any{
		"ok": true, "status": status, "agent": agent,
		"changed": prev != want, "state": state,
	})
}

// serveSSE streams the same status snapshot the poll returns. The first frame
// goes out immediately, which is what makes the header's `ws:` field reach
// `connected` at a predictable point early in the recording — the TUI only
// counts the stream as connected once an event has actually been RECEIVED.
func serveSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	frame, err := json.Marshal(statusPayload())
	if err != nil {
		return
	}
	emit := func() bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !emit() {
		return
	}
	ticker := time.NewTicker(sseFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Re-marshal so a pause taken during the recording shows up on the
			// stream as well as the poll.
			if f, err := json.Marshal(statusPayload()); err == nil {
				frame = f
			}
			if !emit() {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
