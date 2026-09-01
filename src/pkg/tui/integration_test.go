package tui

// T33 (#5424): end-to-end v1 acceptance coverage.
//
// WHY THIS FILE EXISTS, AND WHAT IT IS ALLOWED TO PROVE.
//
// The #4907 tracker was decomposed into client, pane and app tasks small enough
// for short contributor runs. That kept every PR reviewable, and it introduced
// exactly one failure mode: "both halves merged" was mistaken for "the feature
// is wired". Tokens and Events had a complete client AND a complete pane while
// the root app never delivered either message. Every isolated package test
// passed. The panes said `waiting for data` forever.
//
// So the bar for this file is not "the parts return correct values". Every
// other test in this package already establishes that, and none of them would
// have caught the defect above. The bar is that the ASSEMBLED loop is INVOKED:
// each assertion below is written against a fixture that records real HTTP
// traffic from the real root model, so a property can only pass if the request
// was actually issued by the wiring under test. A test that would still pass
// with zero call sites is the defect this task exists to eliminate, not a
// cheaper way to satisfy it.
//
// DETERMINISM. Nothing here sleeps a production duration. The two poll chains
// are driven through model fields (reconcileInterval/activityInterval) that
// exist for this purpose, the SSE stream is driven by a channel the test owns,
// and every wait is a condition poll bounded by a short deadline rather than a
// fixed delay. The suite is required to pass under -count=20 and -shuffle=on.
//
// TMUX IS NOT EXERCISED HERE, DELIBERATELY. `a` builds a tmux command and hands
// it to tea.ExecProcess, which suspends the terminal and attaches to a real
// session. That cannot safely execute in CI — there is no TTY, and a successful
// attach would block the test binary inside an interactive session. The command
// CONSTRUCTION is already owned by attach_test.go/attach_errors_test.go. What
// this file covers is the BINDING AND SELECTION path up to that boundary: that
// `a` is gated on the Agents pane being focused, that it targets the displayed
// selection, and that a second press cannot queue a second suspend. The process
// is never spawned. See TestAttachBindingTargetsSelectionWithoutSpawningTmux.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/panes"
)

// ── The fixture dashboard ────────────────────────────────────────────────────

// recordedRequest is one request the fixture served, captured in enough detail
// that an assertion can cover method, path, body and auth rather than only
// "something was called".
//
// Path is the RAW path (r.URL.EscapedPath()), not the decoded one. That is the
// whole point for the escaping assertions: an agent named "team/one" must reach
// the server as "/api/pause/team%2Fone", and a decoded path would render the
// correct and the broken spelling identically.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
	Auth   string
}

// fixtureDashboard is a deterministic stand-in for the dashboard API: every
// endpoint the v1 TUI consumes, plus a stream the test drives by hand.
//
// It records every request. Handlers are overridable per-endpoint so a single
// property can make one endpoint fail without the others changing behaviour,
// which is what keeps the failure-isolation assertions honest.
type fixtureDashboard struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// bodies maps a path to the JSON body served for it. Guarded by mu so a
	// test can rewrite a response mid-run (the "authoritative response" and
	// poll-fallback properties both need this).
	bodies map[string]string

	// statuses maps a path to a non-200 status to answer with. Absent means
	// 200.
	statuses map[string]int

	// frames is the SSE channel the test publishes on. Each value is one
	// complete event; the handler frames it as `event:`/`data:` lines.
	frames chan sseFrame

	// streamOpens counts how many times /api/events has been dialled. It is
	// how the reconnect properties tell "reconnected once" from "spawned a
	// second reader loop".
	streamOpens int

	// shutdown latches at teardown so a late reconnect cannot park a handler
	// that Server.Close would then wait on forever.
	shutdown bool

	// streamClosed, when closed, makes the CURRENT stream handler return —
	// which is a server-side drop, the thing an operator's flapping dashboard
	// actually does. It is replaced on each new connection.
	streamDrop chan struct{}
}

// sseFrame is one event the fixture publishes.
type sseFrame struct {
	event string
	data  string
}

// The fixture payloads.
//
// These are deliberately DISTINGUISHABLE from the zero value in every field an
// assertion reads: a test that passes against `{}` is a test that proves the
// decode ran, not that the wiring delivered. Agent names, token counts and
// audit actions are all chosen so their presence on screen cannot be a
// coincidence.
const (
	// integrationAgents is the roster. Three agents, one of them with a
	// displayName that differs from its name, because the model picker and the
	// pause path must address the CONFIG KEY and a leaked label would 404.
	integrationAgents = `[
  {"name":"scanner","id":"agt_1","displayName":"Scanner","enabled":true,"managed":false,"backend":"claude","model":"claude-opus-4-5"},
  {"name":"quality","id":"agt_2","displayName":"Quality","enabled":true,"managed":true,"backend":"copilot","model":"gpt-5"},
  {"name":"reviewer","id":"agt_3","displayName":"Reviewer","enabled":false,"managed":false,"backend":"claude","model":"claude-sonnet-4-5"}
]`

	// integrationStatus is the live governor + agent-state snapshot served on
	// GET /api/status and republished on the stream.
	integrationStatus = `{
  "timestamp": "2026-09-01T12:00:00Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": false, "state": "running"},
    {"name": "quality", "enabled": true, "paused": false, "state": "running"},
    {"name": "reviewer", "enabled": false, "paused": false, "state": "stopped"}
  ],
  "governor": {
    "active": true, "mode": "quiet", "issues": 3, "prs": 1,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:05 PM UTC"
  },
  "acmmLevel": 3,
  "acmmLevelConfigured": true
}`

	// integrationStatusBusy is the SAME document with the governor in a
	// different mode and scanner PAUSED. It is what the stream pushes to prove
	// property 2: both changes are invisible to a poll-only frame, so a header
	// reading BUSY can only have come from the stream.
	integrationStatusBusy = `{
  "timestamp": "2026-09-01T12:00:01Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": true, "state": "running"},
    {"name": "quality", "enabled": true, "paused": false, "state": "running"},
    {"name": "reviewer", "enabled": false, "paused": false, "state": "stopped"}
  ],
  "governor": {
    "active": true, "mode": "surge", "issues": 31, "prs": 9,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:06 PM UTC"
  },
  "acmmLevel": 3,
  "acmmLevelConfigured": true
}`

	integrationHiveID          = `{"id":"acceptance-hive"}`
	integrationGovernorConfig  = `{"general_advanced":{"eval_interval_s":900}}`
	integrationTokens          = `{"total_tokens":1500,"total_input":1000,"total_output":500,"by_agent_detail":{"scanner":{"input":1000,"output":500}}}`
	integrationTokensRefreshed = `{"total_tokens":9900,"total_input":7700,"total_output":2200,"by_agent_detail":{"scanner":{"input":7700,"output":2200}}}`
	integrationCost            = `{"estimated":{"total_usd":1.25,"by_agent":[{"name":"scanner","usd":1.25,"source":"estimated"}],"unpriced_models":[]}}`
	integrationAudit           = `{"entries":[
  {"ts":"2026-09-01T12:04:05Z","user":"operator","action":"auditnewest","agent":"scanner"},
  {"ts":"2026-09-01T12:04:04Z","user":"governor","action":"auditmiddle","agent":"quality"},
  {"ts":"2026-09-01T12:04:03Z","user":"operator","action":"auditoldest","agent":"reviewer"}
]}`
	integrationAuditRefreshed = `{"entries":[
  {"ts":"2026-09-01T12:09:05Z","user":"operator","action":"auditrefreshed","agent":"scanner"}
]}`
	integrationModels = `{"backend":"claude","models":["claude-opus-4-5","claude-sonnet-4-5"],"fallback":false,"partial":false}`
	integrationPacks  = `{"packs":[
  {"level":3,"name":"L3","description":"three","agentCount":3,"current":true,"agents":[]},
  {"level":4,"name":"L4","description":"four","agentCount":4,"current":false,"agents":[]}
]}`
)

// integrationToken is the bearer every request must carry. It is asserted on
// rather than merely set, because the auth header is part of the contract this
// harness is here to pin.
const integrationToken = "test-token"

// newFixtureDashboard starts the fixture and points the TUI client at it.
func newFixtureDashboard(t *testing.T) *fixtureDashboard {
	t.Helper()

	f := &fixtureDashboard{
		bodies: map[string]string{
			"/api/agents":                   integrationAgents,
			"/api/status":                   integrationStatus,
			"/api/hive-id":                  integrationHiveID,
			"/api/config/governor":          integrationGovernorConfig,
			"/api/tokens":                   integrationTokens,
			"/api/cost":                     integrationCost,
			"/api/audit":                    integrationAudit,
			"/api/packs":                    integrationPacks,
			"/api/inference/models/claude":  integrationModels,
			"/api/inference/models/copilot": `{"backend":"copilot","models":["gpt-5"],"fallback":false,"partial":false}`,
		},
		statuses:   map[string]int{},
		frames:     make(chan sseFrame, 64),
		streamDrop: make(chan struct{}),
	}

	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	// CLEANUP ORDER IS LOAD-BEARING. httptest.Server.Close blocks until every
	// handler returns, and the SSE handler is a long-lived one that only
	// returns when its drop channel closes or the client's request context is
	// cancelled. Closing the server first therefore deadlocks for the full
	// test timeout — which is what happened the first time this harness ran.
	//
	// Cleanups run LIFO, so this one (registered first) runs LAST: the harness
	// registers its own stop afterwards and so tears the model down first,
	// cancelling the stream request. shutdownStreams is the belt to that
	// braces, releasing any handler still parked if no model owned it.
	t.Cleanup(func() {
		f.shutdownStreams()
		f.Server.Close()
	})
	pinDashboard(t, f.Server.URL)
	return f
}

func (f *fixtureDashboard) serve(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, not Path: the escaping assertions depend on seeing the wire
	// spelling. See recordedRequest.
	path := r.URL.EscapedPath()

	var body []byte
	if r.Body != nil {
		body, _ = readAllLimited(r)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   path,
		Query:  r.URL.RawQuery,
		Body:   strings.TrimSpace(string(body)),
		Auth:   r.Header.Get("Authorization"),
	})
	f.mu.Unlock()

	if path == "/api/events" {
		f.serveStream(w, r)
		return
	}

	// Decoded path for the response table: writes are addressed with escaped
	// segments, and the table is keyed by the logical endpoint.
	f.mu.Lock()
	status, hasStatus := f.statuses[r.URL.Path]
	payload, hasBody := f.bodies[r.URL.Path]
	f.mu.Unlock()

	if hasStatus && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"fixture failure"}`))
		return
	}

	// The mutating endpoints are answered from their path shape rather than
	// from the table, because each one's response must name the agent that was
	// actually addressed — an assertion that the frame renders the
	// AUTHORITATIVE response cannot be written against a constant that ignores
	// the request.
	if resp, ok := f.mutationResponse(r.Method, r.URL.Path, string(body)); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
		return
	}

	if !hasBody {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(payload))
}

// mutationResponse answers the four write endpoints.
//
// Each response echoes the DECODED agent name from the path, which is what
// makes the "renders the authoritative response" assertions meaningful: the
// text on screen can only be right if the escaped path round-tripped correctly.
func (f *fixtureDashboard) mutationResponse(method, path, body string) (string, bool) {
	switch {
	case method == http.MethodPost && strings.HasPrefix(path, "/api/pause/"):
		agent := strings.TrimPrefix(path, "/api/pause/")
		return fmt.Sprintf(`{"ok":true,"status":"paused","agent":%q,"changed":true,"state":"paused"}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/resume/"):
		agent := strings.TrimPrefix(path, "/api/resume/")
		return fmt.Sprintf(`{"ok":true,"status":"resumed","agent":%q,"changed":true,"state":"running"}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/kick/"):
		agent := strings.TrimPrefix(path, "/api/kick/")
		return fmt.Sprintf(`{"status":"queued","agent":%q}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/model/"):
		rest := strings.TrimPrefix(path, "/api/model/")
		agent, modelID, _ := strings.Cut(rest, "/")
		return fmt.Sprintf(`{"status":"ok","agent":%q,"model":%q}`, agent, modelID), true
	case method == http.MethodPut && path == "/api/packs/level":
		var req struct {
			Level int `json:"level"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		return fmt.Sprintf(
			`{"ok":true,"level":%d,"packAgents":["scanner","quality"],"packUpdated":["quality"],"paused":[],"resumed":["quality"]}`,
			req.Level), true
	}
	return "", false
}

// serveStream is the controllable SSE endpoint.
//
// It publishes only what the test pushes onto f.frames — there is no heartbeat
// and no timer — so "the frame moved because of a stream event" is a fact the
// test established rather than a race it won.
func (f *fixtureDashboard) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	f.streamOpens++
	drop := f.streamDrop
	down := f.shutdown
	f.mu.Unlock()
	if down {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case frame := <-f.frames:
			if frame.event != "" {
				if _, err := fmt.Fprintf(w, "event: %s\n", frame.event); err != nil {
					return
				}
			}
			// One data: line per source line — how SSE carries a multi-line
			// payload, and what lets the fixtures above stay readable.
			for _, line := range strings.Split(frame.data, "\n") {
				if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
					return
				}
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-drop:
			// Server-side close: the handler returns, the body ends, and the
			// client's reader sees a clean EOF — which is what a dashboard
			// restarting behind a proxy looks like.
			return
		case <-r.Context().Done():
			return
		}
	}
}

func readAllLimited(r *http.Request) ([]byte, error) {
	const maxBody = 1 << 16
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for len(buf) < maxBody {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
	return buf, nil
}

// ── Fixture accessors ────────────────────────────────────────────────────────

// setBody rewrites the payload served for a path. Used to prove that a later
// poll delivered NEW data rather than re-rendering the first snapshot.
func (f *fixtureDashboard) setBody(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[path] = body
}

// setStatus makes a path fail with a status. Used for the 403 and
// failure-isolation properties.
func (f *fixtureDashboard) setStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[path] = status
}

// countRequests returns how many times a method+path was served. This is the
// CALL COUNT half of the contract: "exactly once" is the assertion that catches
// a duplicate mutation, and no amount of correct rendering substitutes for it.
func (f *fixtureDashboard) countRequests(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			n++
		}
	}
	return n
}

// countPath counts requests to a path regardless of method.
func (f *fixtureDashboard) countPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Path == path {
			n++
		}
	}
	return n
}

// findRequest returns the first recorded request matching method+path.
func (f *fixtureDashboard) findRequest(method, path string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			return req, true
		}
	}
	return recordedRequest{}, false
}

// streamConnections returns how many times /api/events has been dialled.
func (f *fixtureDashboard) streamConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamOpens
}

// shutdownStreams releases every parked stream handler for good.
//
// Unlike dropStream it does NOT install a fresh drop channel: after this, a
// handler that arrives late returns immediately rather than parking, so a
// reconnect racing teardown cannot re-block Server.Close.
func (f *fixtureDashboard) shutdownStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shutdown {
		return
	}
	f.shutdown = true
	close(f.streamDrop)
}

// dropStream closes the current stream server-side and installs a fresh drop
// channel for the next connection.
func (f *fixtureDashboard) dropStream() {
	f.mu.Lock()
	if f.shutdown {
		f.mu.Unlock()
		return
	}
	drop := f.streamDrop
	f.streamDrop = make(chan struct{})
	f.mu.Unlock()
	close(drop)
}

// publish pushes one event onto the stream, failing the test if nothing is
// reading within the deadline (which would mean the app never subscribed).
func (f *fixtureDashboard) publish(t *testing.T, event, data string) {
	t.Helper()
	select {
	case f.frames <- sseFrame{event: event, data: data}:
	case <-time.After(waitTimeout):
		t.Fatal("no reader took the SSE frame: the app never subscribed to /api/events")
	}
}

// ── Driving the real model ───────────────────────────────────────────────────

// The pinned terminal size. 100x30 is comfortably above the 60x20 minimum, so
// every pane has interior rows to render into and an assertion that fails is
// failing about wiring rather than about clipping.
const (
	testTermWidth  = 100
	testTermHeight = 30
)

// waitTimeout bounds every condition wait. It is generous because it is a
// FAILURE deadline, not a delay: a passing assertion returns as soon as its
// condition holds, so raising this cannot slow the suite down — it only decides
// how long a genuinely broken wiring takes to be reported.
const waitTimeout = 10 * time.Second

// pollStep is how often a condition is re-checked. Short enough to be
// invisible, long enough not to spin a core.
const pollStep = time.Millisecond

// harness drives the REAL root model against the fixture.
//
// It is NOT teatest. teatest builds its own program and exposes only the
// rendered byte stream, which is the right tool for the frame-level tests in
// app_test.go and resize_test.go and the wrong one here: this file's properties
// are about which REQUESTS the assembled loop issues and in what number, and it
// must be able to inject a key and then assert on a call count without racing a
// renderer. So it runs the same model through the same Update/View contract
// bubbletea would, on a single goroutine, with commands executed exactly as
// tea.Batch expands them.
//
// The model under test is the one Init() built and every message it produced —
// no message is skipped, and no state is set directly. That is what makes an
// assertion here evidence that the WIRING ran.
type harness struct {
	t *testing.T
	f *fixtureDashboard

	mu    sync.Mutex
	model model

	// msgs is the program's message queue. Commands push onto it from their
	// own goroutines exactly as bubbletea's event loop receives them.
	msgs chan tea.Msg

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once

	// cmdWG tracks in-flight commands so quit can distinguish "the loop
	// stopped" from "goroutines are still running".
	cmdWG sync.WaitGroup

	// quitRequested and execRequested record the two commands this harness
	// recognises but does not run. They live HERE rather than on the model
	// because production behaviour changes are out of scope for this task: the
	// model must stay exactly what ships, so anything the test needs to observe
	// that the model does not already expose is observed at the harness seam.
	quitRequested bool
	execRequested bool
}

// newHarness starts the model's real Init() against the fixture.
//
// Both poll intervals are shortened to a test-only duration. That is the
// mechanism the AC asks for — "short test-only durations, not real sleeps" —
// and both are shortened, not just the reconcile one: since T32 the loops have
// separate timers, and leaving activity at its production 5s would let a test
// that means to exercise the activity cadence silently exercise nothing.
func newHarness(t *testing.T, f *fixtureDashboard, opts ...func(*model)) *harness {
	t.Helper()

	m := newModel()
	m.reconcileInterval = 5 * time.Millisecond
	m.activityInterval = 5 * time.Millisecond
	for _, opt := range opts {
		opt(&m)
	}

	h := &harness{
		t:     t,
		f:     f,
		model: m,
		msgs:  make(chan tea.Msg, 256),
		done:  make(chan struct{}),
	}

	// The window size arrives first, exactly as bubbletea delivers it.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: testTermHeight})

	h.wg.Add(1)
	go h.loop()

	h.exec(m.Init())
	t.Cleanup(h.stop)
	return h
}

// loop is the event loop: one goroutine owning the model, exactly as
// bubbletea's does.
func (h *harness) loop() {
	defer h.wg.Done()
	for {
		select {
		case msg := <-h.msgs:
			h.mu.Lock()
			next, cmd := h.model.Update(msg)
			h.model = next.(model)
			h.mu.Unlock()
			h.exec(cmd)
		case <-h.done:
			return
		}
	}
}

// exec runs a command tree the way bubbletea does: batches expand
// concurrently, each leaf on its own goroutine, and every produced message goes
// back onto the queue.
//
// tea.Quit and tea.ExecProcess are recognised and NOT executed. Quit is
// recorded so the quit property can assert the program asked to exit without
// this harness having to shut down mid-assertion, and ExecProcess would spawn
// the real tmux binary — the boundary this file documents it does not cross.
func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.cmdWG.Add(1)
	go func() {
		defer h.cmdWG.Done()
		msg := cmd()
		switch typed := msg.(type) {
		case tea.BatchMsg:
			for _, sub := range typed {
				h.exec(sub)
			}
			return
		case nil:
			return
		}
		if isQuitMsg(msg) {
			h.mu.Lock()
			h.quitRequested = true
			h.mu.Unlock()
			return
		}
		if isExecMsg(msg) {
			// tea.ExecProcess. Running it would spawn tmux; see the file
			// header. The command's construction is covered by attach_test.go.
			h.mu.Lock()
			h.execRequested = true
			h.mu.Unlock()
			return
		}
		select {
		case h.msgs <- msg:
		case <-h.done:
		}
	}()
}

// send injects a message, as bubbletea's Send does.
func (h *harness) send(msg tea.Msg) {
	select {
	case h.msgs <- msg:
	case <-h.done:
	}
}

// key injects a key press by its string form, which is how app.go's bindings
// are written.
func (h *harness) key(s string) {
	h.send(keyMsg(s))
}

// typeText injects each character as its own rune key, which is what the ACMM
// confirmation field consumes.
func (h *harness) typeText(s string) {
	for _, r := range s {
		if r == ' ' {
			h.send(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// snapshot returns a copy of the current model under the lock.
func (h *harness) snapshot() model {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.model
}

// didQuit reports whether the program asked bubbletea to exit.
func (h *harness) didQuit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quitRequested
}

// view renders the current frame.
func (h *harness) view() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.model.View()
}

// stop ends the loop. Idempotent so both the deferred cleanup and an explicit
// call are safe.
func (h *harness) stop() {
	h.once.Do(func() {
		close(h.done)
		h.wg.Wait()
		// Cancel the live stream request. Without this the fixture's SSE
		// handler stays parked on a connection nobody is reading, and
		// httptest.Server.Close blocks on it until the test timeout — which is
		// exactly the deadlock the first run of this harness hit. It is also
		// what the model's own quit path does (stopSSE), so tearing down here
		// mirrors production rather than working around it.
		h.mu.Lock()
		h.model.cancelSSE()
		h.mu.Unlock()
	})
}

// waitFor blocks until cond holds on the model, failing with why on timeout.
//
// This is the only waiting primitive in the file. There is no sleep-then-assert
// anywhere: a condition that becomes true in a microsecond returns in a
// microsecond, which is what makes -count=20 cheap AND what makes a real
// regression fail loudly instead of flakily.
func (h *harness) waitFor(why string, cond func(model) bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond(h.snapshot()) {
			return
		}
		time.Sleep(pollStep)
	}
	h.t.Fatalf("timed out waiting for %s\nlast frame:\n%s", why, h.view())
}

// waitForView blocks until the rendered frame satisfies cond.
func (h *harness) waitForView(why string, cond func(string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond(h.view()) {
			return
		}
		time.Sleep(pollStep)
	}
	h.t.Fatalf("timed out waiting for %s\nlast frame:\n%s", why, h.view())
}

// waitForFixture blocks until cond holds on the fixture's recorded traffic.
func (h *harness) waitForFixture(why string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollStep)
	}
	h.t.Fatalf("timed out waiting for %s\nlast frame:\n%s", why, h.view())
}

// settle waits for the message queue to drain and in-flight commands to finish.
//
// It is used ONLY before a negative assertion ("exactly once", "no second
// loop"). A positive assertion always uses waitFor instead, because waiting for
// quiet to prove something happened would be a race.
func (h *harness) settle() {
	h.t.Helper()
	// Two consecutive empty observations, because a command can be between
	// producing its message and the loop receiving it.
	for i := 0; i < 2; i++ {
		deadline := time.Now().Add(waitTimeout)
		for len(h.msgs) > 0 && time.Now().Before(deadline) {
			time.Sleep(pollStep)
		}
		time.Sleep(5 * pollStep)
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// isQuitMsg reports whether a message is bubbletea's quit signal. The type is
// unexported by bubbletea, so it is identified by its type name — which is
// stable API in practice (tea.Quit's contract) and is checked by
// TestQuitMsgDetectionMatchesBubbletea below so a bubbletea upgrade that
// renamed it could not silently turn the quit assertions into no-ops.
func isQuitMsg(msg tea.Msg) bool {
	return fmt.Sprintf("%T", msg) == "tea.QuitMsg"
}

func isExecMsg(msg tea.Msg) bool {
	return strings.Contains(fmt.Sprintf("%T", msg), "execMsg")
}

// ── Property 1: startup ──────────────────────────────────────────────────────

// TestStartupLoadsEveryPaneAndBothLiveHeaderFieldsWithoutWaitingAnInterval is
// the regression test for the defect this whole task exists to prevent.
//
// It is deliberately written as ONE test over all four panes plus both live
// header fields, rather than four tests that could each pass while the app
// delivered none of them. Tokens and Events are the two that were broken: they
// had a complete client and a complete pane, and no delivery. So the assertions
// below are on the RENDERED FRAME — the thing an operator sees — not on the
// model's caches, because a cache can be populated by a fetch whose message
// nothing routes to a pane, which is exactly what shipped.
//
// "Without waiting production intervals" is proved by construction: nothing
// here advances a clock, and both intervals are 5ms. If the panes only filled
// on a tick rather than on Init's immediate poll, this would still pass — so
// the call-count assertion at the end pins that too, by requiring the frame to
// be complete while each endpoint has been read a small number of times.
func TestStartupLoadsEveryPaneAndBothLiveHeaderFieldsWithoutWaitingAnInterval(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	// The four panes, each asserted through a value that can only be on screen
	// if that pane's own message was delivered.
	h.waitForView("the Agents pane to render the polled roster", func(v string) bool {
		return strings.Contains(v, "scanner") && strings.Contains(v, "reviewer")
	})
	h.waitForView("the Governor pane to render live governor state", func(v string) bool {
		// The pane case-folds the wire's mode.
		return strings.Contains(strings.ToUpper(v), "QUIET")
	})
	h.waitForView("the Tokens pane to render polled token counts", func(v string) bool {
		// 1000 input renders as a magnitude ("1.0k"); the agent row is the
		// unambiguous proof the pane loaded rather than showing its stub.
		return !strings.Contains(v, "waiting for data") && strings.Contains(v, "scanner")
	})
	h.waitForView("the Events pane to render polled audit rows", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})

	// Both LIVE header fields. `ws:` is excluded on purpose — it is connection
	// state, not data, and property 2 owns it.
	h.waitForView("the header to show the polled hive identity", func(v string) bool {
		return strings.Contains(v, "hive: acceptance-hive")
	})
	h.waitForView("the header to show the polled governor mode", func(v string) bool {
		return strings.Contains(v, "governor: QUIET")
	})

	// The panes are full, so every endpoint that feeds them was actually read.
	// Asserting the traffic as well as the frame is what distinguishes "the
	// wiring ran" from "a pane defaulted to something that looks right".
	for _, path := range []string{
		"/api/agents", "/api/status", "/api/hive-id", "/api/config/governor",
		"/api/tokens", "/api/cost", "/api/audit",
	} {
		if got := f.countPath(path); got == 0 {
			t.Errorf("startup never requested %s: the pane it feeds cannot be live", path)
		}
	}
}

// TestStartupSendsTheBearerTokenOnEveryRequest pins the auth half of the
// contract. It is separate from the property above because a frame can be
// perfectly correct against a fixture that does not check auth, and the real
// dashboard does.
func TestStartupSendsTheBearerTokenOnEveryRequest(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the frame to load", func(v string) bool {
		return strings.Contains(v, "hive: acceptance-hive")
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, req := range f.requests {
		if req.Auth != "Bearer "+integrationToken {
			t.Errorf("%s %s carried Authorization %q, want %q",
				req.Method, req.Path, req.Auth, "Bearer "+integrationToken)
		}
	}
}

// ── Property 2: SSE updates state immediately ────────────────────────────────

// TestStreamEventUpdatesAgentAndGovernorStateImmediately proves the push path
// moves the frame.
//
// The fixture's stream payload CONTRADICTS what /api/agents and /api/status
// return: the governor is in surge rather than quiet, and scanner is paused
// rather than running. That contradiction is the mechanism — a frame showing
// SURGE cannot have come from the poll, so the assertion cannot be satisfied by
// the polling path it is meant to be distinguishing itself from.
func TestStreamEventUpdatesAgentAndGovernorStateImmediately(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	// Wait for the roster: SSE agent states are JOINED onto the polled roster
	// (see paneMsgs), so a frame published before the first fetch resolves
	// carries states with nothing to join them to.
	h.waitForView("the polled roster to arrive before the stream event", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	h.waitForView("the poll-sourced governor mode", func(v string) bool {
		return strings.Contains(v, "governor: QUIET")
	})

	f.publish(t, "", integrationStatusBusy)

	h.waitForView("the header governor mode to follow the stream event", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})
	h.waitFor("the connection state to become connected on the first event", func(m model) bool {
		return m.sseConnected
	})
	h.waitForView("the header ws field to report the live stream", func(v string) bool {
		return strings.Contains(v, "ws: "+wsConnected)
	})
	h.waitFor("the stream's agent states to reach the Agents pane", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		// scanner is running per /api/agents and PAUSED per the stream.
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "scanner" && paused
	})
}

// TestStreamEventDoesNotBlankTheConfiguredGovernorInterval is T29's regression,
// asserted through the assembled loop rather than the pane.
//
// The stream's payload contains no evaluation interval. Before T29 the SSE path
// built its own GovernorMsg with a zero one, so the first pushed event reverted
// `next eval` to unknown. This holds the property at the integration level: the
// interval survives a stream event that never carried it.
func TestStreamEventDoesNotBlankTheConfiguredGovernorInterval(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitFor("the configured eval interval to be polled", func(m model) bool {
		return m.governorInterval == 15*time.Minute
	})
	h.waitForView("the roster to arrive", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	f.publish(t, "", integrationStatusBusy)
	h.waitForView("the stream event to land", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})

	if got := h.snapshot().governorInterval; got != 15*time.Minute {
		t.Fatalf("a stream event carrying no interval overwrote the cached one: got %v, want 15m", got)
	}
	// And the frame still renders it, which is the operator-visible half.
	if v := h.view(); strings.Contains(v, "next eval —") {
		t.Errorf("the Governor pane reverted next eval to unknown after a stream event:\n%s", v)
	}
}

// ── Property 3: the activity cadence survives a healthy stream ───────────────

// TestActivityDataKeepsRefreshingWhileTheStreamIsHealthy is T32's property, and
// the one most likely to silently regress: it is invisible in every isolated
// test, costs nothing when broken, and makes the frame STALER exactly when the
// header claims it is live.
//
// The mechanism is that the fixture's token and audit bodies are REPLACED after
// the stream connects. A frame that keeps polling the activity class will pick
// up the new values; a frame whose activity loop was stretched to 60s (or
// retired by the reconcile generation bump) will sit on the first snapshot
// until this test's deadline expires. There is no sleeping and no clock
// manipulation — only 5ms intervals and a wait for new content.
func TestActivityDataKeepsRefreshingWhileTheStreamIsHealthy(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the first token snapshot", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	h.waitForView("the first audit snapshot", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})

	// Bring the stream up. This is the state that used to stretch everything.
	f.publish(t, "", integrationStatus)
	h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
	h.waitFor("the reconcile loop to stretch, proving the stream is being trusted",
		func(m model) bool { return m.reconcileInterval == sseReconcileInterval })

	// The activity cadence must NOT have moved. This is the direct assertion;
	// the traffic assertions below are the behavioural one.
	if got := h.snapshot().activityInterval; got != 5*time.Millisecond {
		t.Fatalf("a healthy stream changed the activity cadence to %v: the Tokens and Events panes are now staler than when the stream was down", got)
	}

	tokensBefore := f.countPath("/api/tokens")
	auditBefore := f.countPath("/api/audit")

	f.setBody("/api/tokens", integrationTokensRefreshed)
	f.setBody("/api/audit", integrationAuditRefreshed)

	h.waitForView("the Tokens pane to refresh WHILE the stream is healthy", func(v string) bool {
		// 7700 input renders as a magnitude; the refreshed audit action is the
		// unambiguous marker for the Events half.
		return strings.Contains(v, "7.7k")
	})
	h.waitForView("the Events pane to refresh WHILE the stream is healthy", func(v string) bool {
		return strings.Contains(v, "auditrefreshed")
	})

	// The stream is still up: the refresh above happened DESPITE a healthy
	// stream, which is the property, not because the stream dropped.
	if !h.snapshot().sseConnected {
		t.Fatal("the stream dropped during the test, so this proved nothing about the healthy-stream cadence")
	}
	if got := f.countPath("/api/tokens"); got <= tokensBefore {
		t.Errorf("/api/tokens was not re-read while the stream was healthy: %d then, %d now", tokensBefore, got)
	}
	if got := f.countPath("/api/audit"); got <= auditBefore {
		t.Errorf("/api/audit was not re-read while the stream was healthy: %d then, %d now", auditBefore, got)
	}
}

// ── Property 4: drop, fallback, last-good data, single reconnect ─────────────

// TestStreamDropActivatesPollFallbackPreservesDataAndReconnectsOnce covers the
// whole degraded path as one property, because its four halves are only correct
// together: a drop that changed the header but left the poll stretched would
// look fine here if the header were asserted alone.
func TestStreamDropActivatesPollFallbackPreservesDataAndReconnectsOnce(t *testing.T) {
	f := newFixtureDashboard(t)
	// The reconnect backoff is a production constant (1s), so the harness runs
	// with a fast reconcile cadence and simply tolerates the one-second wait
	// for the reconnect assertion — waitTimeout covers it. Nothing SLEEPS for
	// it; the wait ends the moment the stream reconnects.
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	f.publish(t, "", integrationStatusBusy)
	h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
	h.waitFor("the reconcile cadence to stretch", func(m model) bool {
		return m.reconcileInterval == sseReconcileInterval
	})
	h.waitForView("the stream-sourced governor mode", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})

	connectionsBefore := f.streamConnections()
	f.dropStream()

	// (a) The connection state changes, and the header says so.
	h.waitFor("the model to notice the drop", func(m model) bool { return !m.sseConnected })
	h.waitForView("the header to report the stream is down", func(v string) bool {
		return strings.Contains(v, "ws: "+wsNotConnected)
	})

	// (b) The poll fallback is reactivated at the fast cadence.
	h.waitFor("the reconcile cadence to return to the fallback interval", func(m model) bool {
		return m.reconcileInterval == 5*time.Millisecond
	})

	// (c) LAST-GOOD DATA SURVIVES. The panes and the two data header fields
	// keep what they had — a drop changes `ws:`, not what is known.
	view := h.view()
	if !strings.Contains(view, "hive: acceptance-hive") {
		t.Errorf("the hive identity was blanked by a stream drop:\n%s", view)
	}
	if !strings.Contains(view, "scanner") {
		t.Errorf("the Agents pane was cleared by a stream drop:\n%s", view)
	}
	if !strings.Contains(view, "auditnewest") {
		t.Errorf("the Events pane was cleared by a stream drop:\n%s", view)
	}

	// (d) It reconnects, and exactly one reader loop exists afterwards.
	h.waitForFixture("the stream to be re-dialled after the drop", func() bool {
		return f.streamConnections() > connectionsBefore
	})
	h.waitFor("a live stream to be installed again", func(m model) bool {
		return m.sse != nil
	})

	// One event, one delivery. A duplicated reader loop would consume the
	// single frame published here and leave the other loop waiting, or would
	// double-deliver; either way the connection count is the direct evidence.
	f.publish(t, "", integrationStatus)
	h.waitFor("the reconnected stream to be healthy", func(m model) bool { return m.sseConnected })

	h.settle()
	if got, want := f.streamConnections(), connectionsBefore+1; got != want {
		t.Errorf("stream was dialled %d times across one drop, want %d: a second reader loop was armed", got, want)
	}
}

// ── Property 5: focus and navigation target the displayed selection ──────────

// TestFocusCyclesAndNavigationTargetsTheDisplayedSelection covers the two halves
// together because a selection that moves in a pane nobody focused, or a focus
// that moves without changing which pane consumes j/k, are the same bug seen
// from two sides.
func TestFocusCyclesAndNavigationTargetsTheDisplayedSelection(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "reviewer")
	})

	// Focus starts on Agents.
	if got := h.snapshot().focus; got != 0 {
		t.Fatalf("focus started at %d, want 0 (Agents)", got)
	}

	// j moves the Agents selection, and the ACTION KEYS follow it. The
	// selection is read back through the same accessor `p`/`m`/`K`/`a` use, so
	// this asserts what the action would target, not a private cursor.
	h.key("j")
	h.waitFor("the Agents selection to move to the second row", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	h.key("j")
	h.waitFor("the Agents selection to move to the third row", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "reviewer"
	})

	// It clamps at the end rather than wrapping.
	h.key("j")
	h.settle()
	agents, _ := h.snapshot().panes[0].(panes.Agents)
	if name, _, _ := agents.SelectedAgent(); name != "reviewer" {
		t.Errorf("selection wrapped past the last row to %q, want it clamped at reviewer", name)
	}

	h.key("k")
	h.waitFor("k to move the selection back up", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	// tab cycles forward through all four panes and back to Agents.
	for want := 1; want <= 3; want++ {
		h.key("tab")
		h.waitFor(fmt.Sprintf("focus to reach pane %d", want), func(m model) bool {
			return m.focus == want
		})
	}
	h.key("tab")
	h.waitFor("focus to wrap back to Agents", func(m model) bool { return m.focus == 0 })

	// shift+tab cycles backward, and must not go negative.
	h.key("shift+tab")
	h.waitFor("shift+tab to wrap backward to the last pane", func(m model) bool {
		return m.focus == paneCount-1
	})

	// With Events focused, j/k drive the EVENTS pane, not Agents. The Agents
	// selection is the control: it must not move.
	agentsBefore, _ := h.snapshot().panes[0].(panes.Agents)
	nameBefore, _, _ := agentsBefore.SelectedAgent()

	h.waitForView("the Events pane to have rows to scroll", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})
	h.key("j")
	h.waitForView("the Events pane to scroll off the newest entry", func(v string) bool {
		return !strings.Contains(v, "auditnewest") && strings.Contains(v, "auditmiddle")
	})

	agentsAfter, _ := h.snapshot().panes[0].(panes.Agents)
	nameAfter, _, _ := agentsAfter.SelectedAgent()
	if nameAfter != nameBefore {
		t.Errorf("j moved the Agents selection (%q -> %q) while the Events pane was focused", nameBefore, nameAfter)
	}
}

// TestActionKeysAreInertUnlessTheAgentsPaneIsFocused pins the other half of
// "navigation targets the displayed selection": an action addressed at a
// selection the operator cannot see must not fire at all.
func TestActionKeysAreInertUnlessTheAgentsPaneIsFocused(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	// Focus the Governor pane.
	h.key("tab")
	h.waitFor("focus to move off Agents", func(m model) bool { return m.focus == 1 })

	h.key("p")
	h.key("m")
	h.key("K")
	h.settle()

	m := h.snapshot()
	if m.confirm != nil {
		t.Error("p opened the pause dialog while the Agents pane was not focused")
	}
	if m.picker != nil {
		t.Error("m opened the model picker while the Agents pane was not focused")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K issued %d kick requests while the Agents pane was not focused, want 0", got)
	}
}

// ── Property 6: every v1 action, exactly once, correctly escaped ─────────────

// TestPauseConfirmationHitsTheEscapedEndpointExactlyOnceAndRendersTheResponse
// covers the pause action end to end.
//
// The three things it pins are the three that isolated tests cannot: that the
// key opens the dialog, that confirming issues ONE correctly-addressed request,
// and that the frame afterwards shows the SERVER's answer rather than an
// optimistic local guess.
func TestPauseConfirmationHitsTheEscapedEndpointExactlyOnceAndRendersTheResponse(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("p")
	h.waitFor("the pause dialog to open on the selected agent", func(m model) bool {
		return m.confirm != nil && m.confirm.agent == "scanner" && m.confirm.pause
	})
	h.waitForView("the dialog to name the agent it will act on", func(v string) bool {
		return strings.Contains(v, "Pause agent scanner?")
	})

	// Nothing has been sent yet: opening a confirmation must not write.
	if got := f.countRequests(http.MethodPost, "/api/pause/scanner"); got != 0 {
		t.Fatalf("opening the dialog already issued %d pause requests, want 0", got)
	}

	// Hammer y. Only the first may reach the server; the rest land while the
	// request is pending and must be refused.
	h.key("y")
	h.key("y")
	h.key("y")

	h.waitFor("the pause to complete and close the dialog", func(m model) bool {
		return m.confirm == nil
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/pause/scanner"); got != 1 {
		t.Errorf("pause issued %d requests, want exactly 1: a pending command did not block repeats", got)
	}
	req, ok := f.findRequest(http.MethodPost, "/api/pause/scanner")
	if !ok {
		t.Fatal("no POST /api/pause/scanner was recorded")
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("pause carried Authorization %q, want the bearer token", req.Auth)
	}
	if req.Body != "" {
		t.Errorf("pause sent body %q, want none: the operation declares no requestBody", req.Body)
	}

	// The AUTHORITATIVE response is what the row shows. The fixture answered
	// state=paused, so the pane must render scanner as paused even though
	// /api/agents still reports it enabled.
	h.waitFor("the Agents row to take the server's post-call state", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "scanner" && paused
	})
}

// TestPauseForbiddenKeepsTheDialogOpenWithTheOwnerMessage covers the one error
// an operator must be told apart from an outage: a working request from someone
// whose role does not permit it. Retrying will never help, so the wording is
// part of the contract.
func TestPauseForbiddenKeepsTheDialogOpenWithTheOwnerMessage(t *testing.T) {
	f := newFixtureDashboard(t)
	f.setStatus("/api/pause/scanner", http.StatusForbidden)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("p")
	h.waitFor("the dialog to open", func(m model) bool { return m.confirm != nil })
	h.key("y")

	h.waitForView("the 403 to be rendered as an owner-access message", func(v string) bool {
		return strings.Contains(v, "owner access required")
	})
	if h.snapshot().confirm == nil {
		t.Error("the dialog closed on a 403: the operator cannot read the failure or retry")
	}
	// A failed write must not move the row.
	agents, _ := h.snapshot().panes[0].(panes.Agents)
	if _, paused, _ := agents.SelectedAgent(); paused {
		t.Error("a forbidden pause marked the agent paused: the write did not happen")
	}
}

// TestResumeUsesTheResumeEndpointForAPausedAgent proves the dialog's verb is
// derived from the row's live state rather than hardcoded — a pause key that
// always POSTed /api/pause would be undetectable in a fixture whose agents are
// all running.
func TestResumeUsesTheResumeEndpointForAPausedAgent(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "reviewer")
	})

	// reviewer is enabled:false, which the Agents pane reads as paused.
	h.key("j")
	h.key("j")
	h.waitFor("the selection to reach the disabled agent", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "reviewer" && paused
	})

	h.key("p")
	h.waitFor("the dialog to offer RESUME for a paused agent", func(m model) bool {
		return m.confirm != nil && m.confirm.agent == "reviewer" && !m.confirm.pause
	})
	h.key("y")

	h.waitForFixture("the resume endpoint to be called", func() bool {
		return f.countRequests(http.MethodPost, "/api/resume/reviewer") == 1
	})
	h.settle()
	if got := f.countRequests(http.MethodPost, "/api/pause/reviewer"); got != 0 {
		t.Errorf("resuming a paused agent POSTed /api/pause %d times: the verb is not derived from live state", got)
	}
}

// TestKickHitsTheEscapedEndpointExactlyOnceAndRendersTheQueuedStatus covers `K`.
//
// The kick response is a QUEUEING receipt, not a delivery confirmation, and the
// footer wording is part of that contract (#5325) — an operator told "kicked"
// would believe the prompt was typed.
func TestKickHitsTheEscapedEndpointExactlyOnceAndRendersTheQueuedStatus(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	// Three presses; only one request may leave, because kickPending bounds it.
	h.key("K")
	h.key("K")
	h.key("K")

	h.waitForView("the footer to report the kick was queued", func(v string) bool {
		return strings.Contains(v, "kick queued for scanner")
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/kick/scanner"); got != 1 {
		t.Errorf("kick issued %d requests, want exactly 1: repeated presses were not bounded", got)
	}
	req, _ := f.findRequest(http.MethodPost, "/api/kick/scanner")
	if req.Body != "" {
		t.Errorf("kick sent body %q, want none: an empty prompt must reach the auto-generated-message path", req.Body)
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("kick carried Authorization %q, want the bearer token", req.Auth)
	}
}

// TestModelPickerFetchesTheCatalogueAndAppliesExactlyOnce covers `m` and enter.
//
// The apply is the widest per-agent write in the API — it RESTARTS the agent's
// session — so "exactly once" is not a tidiness assertion here.
func TestModelPickerFetchesTheCatalogueAndAppliesExactlyOnce(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("m")
	h.waitFor("the picker to open for the selected agent", func(m model) bool {
		return m.picker != nil && m.picker.Agent() == "scanner"
	})

	// The catalogue is fetched for the agent's CONFIGURED BACKEND, not its
	// name — a leaked agent name here would 404 on the routing table.
	h.waitForFixture("the catalogue to be fetched for the claude backend", func() bool {
		return f.countRequests(http.MethodGet, "/api/inference/models/claude") == 1
	})
	h.waitFor("the catalogue to populate the overlay", func(m model) bool {
		return m.picker != nil && !m.picker.Loading()
	})

	// Move off the current model so the apply is a real change, then hammer
	// enter: Apply() refuses while pending, which is what bounds the write.
	h.key("j")
	h.waitFor("the picker selection to move", func(m model) bool {
		sel, ok := m.picker.Selected()
		return ok && sel == "claude-sonnet-4-5"
	})

	h.key("enter")
	h.key("enter")
	h.key("enter")

	h.waitForView("the footer to report the applied model and the session restart", func(v string) bool {
		return strings.Contains(v, "scanner now on claude-sonnet-4-5") &&
			strings.Contains(v, "session restarted")
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/model/scanner/claude-sonnet-4-5"); got != 1 {
		t.Errorf("model apply issued %d requests, want exactly 1: a pending write did not block repeats", got)
	}
	// The overlay closes on success and the row takes the server's answer.
	if h.snapshot().picker != nil {
		t.Error("the picker stayed open after a successful apply")
	}
	h.waitFor("the Agents row to show the authoritative applied model", func(m model) bool {
		return strings.Contains(m.View(), "claude-sonnet-4-5")
	})
}

// TestACMMTypedConfirmationAppliesExactlyOnceAtTheEscapedEndpoint covers `A`.
//
// This is the widest write in the API — it reconciles the whole fleet — and its
// guard is a TYPED phrase rather than a keystroke. The assertions therefore
// cover the refusal path as well as the success path: a wrong phrase that
// applied would defeat the entire control.
func TestACMMTypedConfirmationAppliesExactlyOnceAtTheEscapedEndpoint(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("A")
	h.waitFor("the ACMM overlay to open", func(m model) bool { return m.acmm != nil })
	h.waitForFixture("the pack list to be fetched", func() bool {
		return f.countRequests(http.MethodGet, "/api/packs") >= 1
	})
	h.waitFor("the pack list to populate the overlay", func(m model) bool {
		return m.acmm != nil && !m.acmm.Loading()
	})

	// Move to L4 (L3 is current, and enter on the current level is a no-op by
	// design) and begin the confirmation.
	h.key("j")
	h.waitFor("the selection to reach L4", func(m model) bool {
		pack, ok := m.acmm.SelectedPack()
		return ok && pack.Level == 4
	})
	h.key("enter")
	h.waitFor("the overlay to enter the typed confirmation state", func(m model) bool {
		return m.acmm != nil && m.acmm.Confirming()
	})

	// A WRONG phrase must not apply. This is asserted before the right one so
	// a broken guard fails here rather than being masked by the success below.
	h.typeText("APPLY L5")
	h.waitFor("the wrong phrase to be typed into the field", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == "APPLY L5"
	})
	h.key("enter")
	h.settle()
	if got := f.countRequests(http.MethodPut, "/api/packs/level"); got != 0 {
		t.Fatalf("a wrong confirmation phrase issued %d applies, want 0", got)
	}

	// Clear it and type the exact phrase.
	for range "APPLY L5" {
		h.key("backspace")
	}
	h.waitFor("the field to be cleared", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == ""
	})

	want := panes.ConfirmPhrase(4)
	h.typeText(want)
	h.waitFor("the exact phrase to be typed", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == want
	})

	h.key("enter")
	h.key("enter")
	h.key("enter")

	h.waitFor("the apply to complete and the overlay to hold the receipt", func(m model) bool {
		return m.acmm != nil && m.acmm.Done()
	})
	h.settle()

	if got := f.countRequests(http.MethodPut, "/api/packs/level"); got != 1 {
		t.Errorf("ACMM apply issued %d requests, want exactly 1", got)
	}
	req, ok := f.findRequest(http.MethodPut, "/api/packs/level")
	if !ok {
		t.Fatal("no PUT /api/packs/level recorded")
	}
	// The BODY is the contract here: the level is a required request-body
	// field, not a path segment.
	if req.Body != `{"level":4}` {
		t.Errorf("ACMM apply sent body %q, want %q", req.Body, `{"level":4}`)
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("ACMM apply carried Authorization %q, want the bearer token", req.Auth)
	}
	h.waitForView("the footer to report the authoritative new level", func(v string) bool {
		return strings.Contains(v, "ACMM level now L4")
	})
}

// TestActionsEscapeAgentNamesIntoPathSegments is the escaping assertion, and it
// needs an agent name that a raw interpolation would mangle.
//
// A name containing a slash interpolated raw produces "/api/pause/team/one",
// which matches no route and comes back as a 404 — a failure an operator would
// read as "the dashboard is broken". The fixture records the RAW path, so the
// correct and the broken spelling are distinguishable.
func TestActionsEscapeAgentNamesIntoPathSegments(t *testing.T) {
	f := newFixtureDashboard(t)
	f.setBody("/api/agents", `[
  {"name":"team/one","id":"agt_9","displayName":"Team One","enabled":true,"managed":true,"backend":"claude","model":"claude-opus-4-5"}
]`)
	h := newHarness(t, f)

	h.waitFor("the awkwardly-named agent to be selectable", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == "team/one"
	})

	h.key("K")
	h.waitForView("the kick to complete", func(v string) bool {
		return strings.Contains(v, "kick queued for team/one")
	})
	h.settle()

	escaped := "/api/kick/" + url.PathEscape("team/one")
	if got := f.countRequests(http.MethodPost, escaped); got != 1 {
		t.Errorf("kick did not address the escaped path %s exactly once (got %d)", escaped, got)
	}
	if got := f.countRequests(http.MethodPost, "/api/kick/team/one"); got != 0 {
		t.Errorf("kick sent a RAW-interpolated path %d times: the name leaked into the route", got)
	}
}

// TestAttachBindingTargetsSelectionWithoutSpawningTmux is the tmux boundary.
//
// THE PROCESS IS NEVER SPAWNED. `a` ends in tea.ExecProcess, which suspends the
// terminal and attaches to a real tmux session — impossible in CI, where there
// is no TTY, and actively harmful if it succeeded, because the test binary
// would block inside an interactive session. The command's CONSTRUCTION is
// already owned by attach_test.go and attach_errors_test.go.
//
// What is asserted here is the part those tests cannot see: the BINDING and
// SELECTION path. That `a` is gated on the Agents pane being focused, that it
// marks the attach pending so a second press cannot queue a second
// terminal-suspending command, and that it targets the row the operator can
// see. The harness recognises the exec message and records it instead of
// running it — see harness.exec.
func TestAttachBindingTargetsSelectionWithoutSpawningTmux(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	// Move the selection so the assertion is about the DISPLAYED row rather
	// than about the first row happening to be right.
	h.key("j")
	h.waitFor("the selection to move to quality", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	h.key("a")
	h.waitFor("the attach to be marked pending by the binding", func(m model) bool {
		return m.attachPending
	})

	// A second press while pending must not queue a second suspend.
	h.key("a")
	h.key("a")
	h.settle()
	if !h.snapshot().attachPending {
		t.Error("attachPending was cleared without a result: repeated presses are no longer bounded")
	}

	// The preflight resolves against a PATH with no tmux, so it fails and the
	// failure becomes UI rather than a process-ending error. That is the
	// boundary: the binding and its target are proved, the session is not.
	h.waitForView("the attach preflight failure to be rendered in the footer", func(v string) bool {
		return strings.Contains(v, "Attach failed")
	})
	h.waitFor("the pending flag to clear once the attempt resolved", func(m model) bool {
		return !m.attachPending
	})
}

// ── Property 7: help/footer parity and modal key containment ─────────────────

// TestHelpAndFooterListTheSameAvailableBindings is the parity assertion.
//
// Both lists are hand-maintained transcriptions of the design doc's §4 table —
// there is no runtime registry to derive either from — so they can drift, and
// the drift is invisible: an operator reads help, presses the key, nothing
// happens. This compares the two directly.
func TestHelpAndFooterListTheSameAvailableBindings(t *testing.T) {
	help := panes.Help()

	// Every binding help calls AVAILABLE must be reachable from the footer
	// strip, and must actually be handled by the app.
	for _, b := range panes.HelpBindings() {
		if !b.Available {
			continue
		}
		for _, key := range splitBindingKeys(b.Keys) {
			if !footerAdvertises(key) {
				t.Errorf("help lists %q as available but the footer strip does not advertise it\nfooter: %s", key, footerText)
			}
		}
	}

	// And the reverse: nothing in the footer may be absent from help, because
	// help is where an operator goes to find out what a key does.
	for _, key := range footerKeys() {
		if !helpAdvertises(help, key) {
			t.Errorf("the footer advertises %q but the help overlay does not list it", key)
		}
	}
}

// splitBindingKeys splits a help row's key column ("tab / shift+tab",
// "j / k, ↓ / ↑") into individual keys.
func splitBindingKeys(keys string) []string {
	fields := strings.FieldsFunc(keys, func(r rune) bool {
		return r == '/' || r == ','
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// footerKeys is the leading key token of each "key label" pair in footerText.
func footerKeys() []string {
	fields := strings.Fields(footerText)
	var out []string
	for i := 0; i < len(fields); i += 2 {
		out = append(out, fields[i])
	}
	return out
}

// footerAdvertises reports whether the footer strip mentions a key.
//
// The footer is terser than help by design — it lists `tab` for the focus pair
// and omits the arrow-key aliases and the pane-local j/k — so the comparison is
// on the keys the footer chose to name, with the aliases mapped onto their
// primary. Spelling the aliases out here rather than loosening the match is
// what keeps the test able to fail.
func footerAdvertises(key string) bool {
	switch key {
	case "shift+tab":
		key = "tab"
	case "ctrl+c":
		key = "q"
	case "↓", "↑", "j", "k":
		// Pane-local movement. The footer names the panes' actions, not their
		// cursors, and help marks these as pane-scoped rather than global.
		return true
	}
	for _, f := range footerKeys() {
		if f == key {
			return true
		}
	}
	return false
}

func helpAdvertises(help, key string) bool {
	for _, b := range panes.HelpBindings() {
		for _, k := range splitBindingKeys(b.Keys) {
			if k == key {
				return b.Available
			}
		}
	}
	return strings.Contains(help, key)
}

// TestModalKeysCannotLeakIntoGlobalActions is the containment property, and it
// is the one with real consequences: `A`, `p`, `a` and `K` all appear in
// "APPLY L4", so a key leaking out of the ACMM confirmation would not merely
// fail to type — it would pause an agent or fire a kick while the operator was
// spelling out a fleet-wide change.
func TestModalKeysCannotLeakIntoGlobalActions(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("A")
	h.waitFor("the ACMM overlay to open", func(m model) bool { return m.acmm != nil })
	h.waitFor("the pack list to load", func(m model) bool {
		return m.acmm != nil && !m.acmm.Loading()
	})
	h.key("j")
	h.key("enter")
	h.waitFor("the confirmation field to open", func(m model) bool {
		return m.acmm != nil && m.acmm.Confirming()
	})

	// Type the phrase whose letters are all bindings, plus q and K.
	h.typeText("APPLY L4")
	h.key("K")
	h.key("q")
	h.settle()

	m := h.snapshot()
	if m.acmm == nil {
		t.Fatal("the ACMM overlay closed: a key leaked out of the confirmation")
	}
	if m.confirm != nil {
		t.Error("a letter typed into the ACMM confirmation opened the pause dialog")
	}
	if m.picker != nil {
		t.Error("a letter typed into the ACMM confirmation opened the model picker")
	}
	if h.didQuit() {
		t.Error("q typed into the ACMM confirmation quit the program")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K typed into the ACMM confirmation issued %d kicks, want 0", got)
	}
	// The letters that belong in the phrase are TEXT; K and q are swallowed.
	if got := m.acmm.Typed(); got != "APPLY L4" {
		t.Errorf("the confirmation field holds %q, want %q: keys were consumed wrongly", got, "APPLY L4")
	}
}

// TestHelpOverlaySwallowsQuitInTheAssembledLoop is the same containment property for the overlay
// an operator is most likely to be reading when they press a key at random.
func TestHelpOverlaySwallowsQuitInTheAssembledLoop(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the frame to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("?")
	h.waitFor("the help overlay to open", func(m model) bool { return m.helpVisible })

	// q dismisses the overlay and MUST NOT quit.
	h.key("q")
	h.waitFor("the overlay to be dismissed", func(m model) bool { return !m.helpVisible })
	h.settle()
	if h.didQuit() {
		t.Error("q dismissed the help overlay AND quit the program: the reader lost their session")
	}
}

// TestPauseDialogSwallowsGlobalKeys covers the third modal.
func TestPauseDialogSwallowsGlobalKeys(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("p")
	h.waitFor("the dialog to open", func(m model) bool { return m.confirm != nil })

	focusBefore := h.snapshot().focus
	h.key("q")
	h.key("tab")
	h.key("A")
	h.key("K")
	h.settle()

	m := h.snapshot()
	if h.didQuit() {
		t.Error("q quit the program from inside the pause confirmation")
	}
	if m.focus != focusBefore {
		t.Errorf("tab moved focus from inside the pause confirmation (%d -> %d)", focusBefore, m.focus)
	}
	if m.acmm != nil {
		t.Error("A opened the ACMM overlay from inside the pause confirmation")
	}
	if m.confirm == nil {
		t.Fatal("the confirmation was dismissed by a key that is not n or esc")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K issued %d kicks from inside the pause confirmation, want 0", got)
	}
}

// ── Property 8: resize across the minimum ────────────────────────────────────

// TestResizeBelowAndAboveTheMinimumSwapsTheFrameCleanly proves the floor is a
// SWAP rather than a shrink, and — the half a size-only test cannot see — that
// crossing it does not cost the frame its data.
func TestResizeBelowAndAboveTheMinimumSwapsTheFrameCleanly(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the full frame to load", func(v string) bool {
		return strings.Contains(v, "scanner") && strings.Contains(v, "hive: acceptance-hive")
	})

	// Below the minimum in WIDTH.
	h.send(tea.WindowSizeMsg{Width: minWidth - 1, Height: testTermHeight})
	h.waitForView("the too-small frame to replace the grid", func(v string) bool {
		return strings.Contains(v, "terminal too small")
	})
	small := h.view()
	if strings.Contains(small, "scanner") {
		t.Error("the too-small frame still drew the grid: the floor shrinks rather than swaps")
	}
	if strings.Contains(small, footerText) {
		t.Error("the too-small frame still drew the footer strip")
	}
	// It must fit the terminal exactly rather than overflowing it.
	for i, line := range strings.Split(small, "\n") {
		if w := len([]rune(line)); w > minWidth-1 {
			t.Errorf("too-small frame line %d is %d columns wide, want at most %d", i, w, minWidth-1)
		}
	}

	// Back above the minimum: the full frame returns WITH its data intact.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: testTermHeight})
	h.waitForView("the full frame to return with its data", func(v string) bool {
		return strings.Contains(v, "scanner") &&
			strings.Contains(v, "hive: acceptance-hive") &&
			strings.Contains(v, "auditnewest")
	})
	if strings.Contains(h.view(), "terminal too small") {
		t.Error("the too-small message survived a resize back above the minimum")
	}

	// Below the minimum in HEIGHT only, which is the other half of the guard.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: minHeight - 1})
	h.waitForView("the height floor to swap the frame too", func(v string) bool {
		return strings.Contains(v, "terminal too small")
	})

	// Exactly at the minimum is ABOVE the floor, not below it.
	h.send(tea.WindowSizeMsg{Width: minWidth, Height: minHeight})
	h.waitForView("the minimum supported size to render the full frame", func(v string) bool {
		return !strings.Contains(v, "terminal too small")
	})
}

// ── Property 9: quit cancels the stream without leaking ──────────────────────

// TestQuitCancelsTheStreamWithoutLeakingAGoroutineOrReconnect covers both quit
// keys.
//
// The leak this guards is real and specific: cancelling is what stops the
// goroutine client.StreamEvents owns, and RETIRING THE GENERATION is what stops
// the resulting drop from being mistaken for a real one and scheduling a
// reconnect on the way out. Under `hivectl tui` a leaked goroutine is harmless
// because the process is exiting; under a test binary — or an embedding program
// — it is not.
func TestQuitCancelsTheStreamWithoutLeakingAGoroutineOrReconnect(t *testing.T) {
	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			f := newFixtureDashboard(t)
			h := newHarness(t, f)

			h.waitForView("the frame to load", func(v string) bool {
				return strings.Contains(v, "scanner")
			})
			f.publish(t, "", integrationStatus)
			h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
			h.waitFor("a live stream to be installed", func(m model) bool { return m.sse != nil })

			connectionsBefore := f.streamConnections()
			before := runtime.NumGoroutine()

			h.key(key)

			h.waitForFixture("the program to ask to quit", h.didQuit)
			h.waitFor("the stream to be released on quit", func(m model) bool { return m.sse == nil })

			h.settle()

			// NO RECONNECT. The cancellation closes the channels, so the pump
			// produces one last drop; without the generation bump that drop is
			// indistinguishable from a real one and would schedule a reconnect
			// while the program is already quitting.
			if got := f.streamConnections(); got != connectionsBefore {
				t.Errorf("the stream was re-dialled %d time(s) after quit: the quit path scheduled a reconnect",
					got-connectionsBefore)
			}

			// NO LEAKED GOROUTINE. The stream reader must have exited.
			h.stop()
			h.cmdWG.Wait()
			assertGoroutinesSettle(t, before)
		})
	}
}

// assertGoroutinesSettle waits for the goroutine count to come back to around
// its pre-test level.
//
// A tolerance is used rather than an exact match because the Go runtime and
// net/http keep their own pools, and httptest's own connection handlers wind
// down on their own schedule. The leak this is written to catch is a PER-STREAM
// reader that never exits — one per connection, growing without bound — which a
// small tolerance still detects.
func assertGoroutinesSettle(t *testing.T, before int) {
	t.Helper()
	const tolerance = 4
	deadline := time.Now().Add(waitTimeout)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+tolerance {
			return
		}
		time.Sleep(10 * pollStep)
	}
	t.Errorf("goroutines did not settle after quit: %d before, %d after (tolerance %d)", before, after, tolerance)
}

// ── Harness self-checks ──────────────────────────────────────────────────────

// TestQuitMsgDetectionMatchesBubbletea guards the harness itself.
//
// isQuitMsg identifies bubbletea's quit signal by type name because the type is
// unexported. If a bubbletea upgrade renamed it, every quit assertion in this
// file would silently become a no-op — the exact class of vacuous test this
// task exists to eliminate. This fails instead.
func TestQuitMsgDetectionMatchesBubbletea(t *testing.T) {
	if !isQuitMsg(tea.Quit()) {
		t.Fatalf("isQuitMsg does not recognise tea.Quit() (%T): every quit assertion in this file is vacuous", tea.Quit())
	}
	if isQuitMsg(tea.WindowSizeMsg{}) {
		t.Error("isQuitMsg matched a non-quit message")
	}
}

// TestFixtureRecordsMethodPathBodyAndAuth guards the recording fixture.
//
// Every "exactly once" and "correct endpoint" assertion in this file is only as
// good as this recording. A fixture that silently stopped recording — or
// recorded a decoded path — would turn the escaping and call-count properties
// into tautologies.
func TestFixtureRecordsMethodPathBodyAndAuth(t *testing.T) {
	f := newFixtureDashboard(t)

	// The raw path must survive: this is what makes the escaping assertion
	// able to fail.
	resp, err := http.Get(f.Server.URL + "/api/kick/team%2Fone")
	if err != nil {
		t.Fatalf("fixture request failed: %v", err)
	}
	_ = resp.Body.Close()

	if got := f.countPath("/api/kick/team%2Fone"); got != 1 {
		t.Errorf("the fixture recorded %d requests for the escaped path, want 1: paths are being decoded before recording", got)
	}
	if got := f.countPath("/api/kick/team/one"); got != 0 {
		t.Errorf("the fixture recorded the DECODED path %d times: escaping assertions cannot fail", got)
	}
}
