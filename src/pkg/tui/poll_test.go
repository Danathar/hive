package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/client"
	"github.com/kubestellar/hive/pkg/tui/panes"
)

// agentsFixture is the /api/agents body the poll tests serve. Three agents so
// a decoded result is distinguishable from a zero value at a glance.
const agentsFixture = `[
  {"name":"scanner","id":"agt_1","displayName":"Scanner","enabled":true,"managed":false,"backend":"claude","model":"claude-opus-4-5"},
  {"name":"quality","id":"agt_2","displayName":"Quality","enabled":true,"managed":true,"backend":"copilot","model":"gpt-5"},
  {"name":"reviewer","id":"agt_3","displayName":"Reviewer","enabled":false,"managed":false,"backend":"claude","model":"claude-sonnet-4-5"}
]`

// closedDashboard is an address nothing listens on, used by the tests that
// must not reach a dashboard at all.
//
// Every test in this package pins the client's environment, successful or not.
// The model builds its client from HIVE_DASHBOARD_URL, so a suite that left it
// alone would poll whatever the developer running the tests happens to have on
// localhost:3001 — which is exactly the machine a hive developer is working on.
// The design doc's testing convention is that no task needs a running Hive.
const closedDashboard = "http://127.0.0.1:1"

// TestMain pins the dashboard URL for EVERY test in this package.
//
// The model builds its client from the environment at construction, and T12 is
// the change that made the model start issuing requests on its own. Without
// this, running the suite on a developer's machine — the machine most likely to
// have a hive on localhost:3001 — would poll their real dashboard, and the
// frame-level tests would render whatever it returned. The design doc's testing
// convention is that no task requires a running Hive; this is what keeps that
// true now that the app polls. Tests that need a live server override it with
// t.Setenv, which restores this value afterwards.
func TestMain(m *testing.M) {
	if err := os.Setenv(client.BaseURLEnv, closedDashboard); err != nil {
		panic(err)
	}
	if err := os.Setenv(client.TokenEnv, "test-token"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// pinDashboard points the client at url for the duration of the test, and
// gives it a token so the request shape does not depend on the developer's
// own HIVE_DASHBOARD_TOKEN either.
func pinDashboard(t *testing.T, url string) {
	t.Helper()
	t.Setenv(client.BaseURLEnv, url)
	t.Setenv(client.TokenEnv, "test-token")
}

// pollTestModel is a model pointed at url with a poll interval short enough
// that a tick can be run to completion inside a test.
//
// The interval is what makes these tests fast without sleeping a real one: the
// AC asks for tick scheduling covered without waiting out the cadence, and the
// interval being a model field rather than a bare constant read is what allows
// it. It is deliberately not zero — a zero-duration tea.Tick is a hot loop, and
// a test that passed with one would hide exactly the bug
// TestNewModelPollsOnAnInterval guards.
func pollTestModel(t *testing.T, url string) model {
	t.Helper()
	pinDashboard(t, url)
	m := newModel()
	m.interval = time.Millisecond
	return m
}

// drain runs a tea.Cmd to completion and returns every message it produced,
// flattening tea.Batch.
//
// Batched commands run concurrently under bubbletea with no ordering
// guarantee, so callers assert on MEMBERSHIP, never on position. Running them
// sequentially here is safe precisely because nothing may depend on the order.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		out = append(out, drain(c)...)
	}
	return out
}

// runTick injects a tick into m and returns every message the resulting
// command produced. The tick is INJECTED, never waited for — that is the AC's
// "does not sleep real intervals", and it is also what makes these assertions
// deterministic rather than timing-dependent.
func runTick(m model) []tea.Msg {
	_, cmd := m.Update(tickMsg{})
	return drain(cmd)
}

// findAgentsMsg returns the delivered agent list, or nil if no AgentsMsg was
// produced.
func findAgentsMsg(msgs []tea.Msg) *panes.AgentsMsg {
	for _, m := range msgs {
		if a, ok := m.(panes.AgentsMsg); ok {
			return &a
		}
	}
	return nil
}

func hasTick(msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(tickMsg); ok {
			return true
		}
	}
	return false
}

func findFetchErr(msgs []tea.Msg) *fetchErrMsg {
	for _, m := range msgs {
		if e, ok := m.(fetchErrMsg); ok {
			return &e
		}
	}
	return nil
}

// agentsServer serves the fixture, or a 500 once fail is set. The counter lets
// a test assert that a second tick really issued a second request rather than
// replaying a cached result.
//
// It counts /api/agents ONLY. Since T13b the model also subscribes to
// /api/events at startup, and that request lands on this handler too — so a
// counter that counted every path would make "the poll fetched twice" and "the
// poll fetched once and the stream connected" indistinguishable, and would
// depend on when an asynchronous stream goroutine happened to dial.
type agentsServer struct {
	*httptest.Server
	fail     atomic.Bool
	requests atomic.Int64
}

func newAgentsServer(t *testing.T) *agentsServer {
	t.Helper()
	s := &agentsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.requests.Add(1)
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(agentsFixture))
	}))
	t.Cleanup(s.Close)
	return s
}

// TestNewModelPollsOnAnInterval guards the cheapest catastrophic mistake in
// this file: an interval left at its zero value.
//
// tea.Tick(0, …) fires immediately and forever, so a model that forgot to set
// the field would spin the CPU and hammer the dashboard as fast as the network
// allows — while every other test in this package still passed, because they
// all override the interval themselves.
func TestNewModelPollsOnAnInterval(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()

	if m.interval <= 0 {
		t.Fatalf("newModel().interval = %v, want a positive cadence (a zero tick is a hot loop)", m.interval)
	}
	if m.interval != pollInterval {
		t.Errorf("newModel().interval = %v, want pollInterval (%v)", m.interval, pollInterval)
	}
	if m.api == nil {
		t.Fatal("newModel() built no client; poll would nil-panic on the first tick")
	}
}

// TestInitPollsImmediatelyAndArmsTick pins that startup does not wait out a
// full interval before showing anything. Without the immediate fetch every
// pane would sit on "waiting for data" for five seconds against a perfectly
// healthy dashboard, which an operator reads as the TUI being broken.
func TestInitPollsImmediatelyAndArmsTick(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.Init())

	got := findAgentsMsg(msgs)
	if got == nil {
		t.Fatalf("Init() issued no agents fetch; produced %d messages", len(msgs))
	}
	if len(got.Agents) != 3 {
		t.Errorf("Init() delivered %d agents, want 3", len(got.Agents))
	}
	if !hasTick(msgs) {
		t.Error("Init() armed no tick; the poll loop would never run a second time")
	}
	if n := server.requests.Load(); n != 1 {
		t.Errorf("Init() made %d requests, want exactly 1", n)
	}
}

// TestTickFetchesAndRearms is the AC's tick-scheduling test: the tick message
// is INJECTED rather than waited for, so nothing here sleeps an interval.
//
// One tick must do both things — issue the fetches and arm the next tick. A
// handler that only fetched would poll exactly once and then go quiet, and a
// static frame is indistinguishable from a live one showing unchanged data.
func TestTickFetchesAndRearms(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	next, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("a tick produced no command at all")
	}
	if _, ok := next.(model); !ok {
		t.Fatalf("a tick returned %T, want the root model", next)
	}

	msgs := drain(cmd)
	if got := findAgentsMsg(msgs); got == nil || len(got.Agents) != 3 {
		t.Errorf("a tick did not deliver the agent list: %+v", got)
	}
	if !hasTick(msgs) {
		t.Error("a tick did not arm the next one; the loop stops after one poll")
	}
}

// TestTickRearmsWhenTheFetchFails is the half of the error policy that keeps
// the loop alive. A dashboard that is down must not be able to stop the clock:
// if the re-arm were chained off a successful fetch, one 500 would end polling
// for the rest of the session and the TUI would never notice the dashboard
// coming back.
func TestTickRearmsWhenTheFetchFails(t *testing.T) {
	server := newAgentsServer(t)
	server.fail.Store(true)
	m := pollTestModel(t, server.URL)

	msgs := runTick(m)

	if !hasTick(msgs) {
		t.Fatal("a failed fetch stopped the tick loop")
	}
	if findFetchErr(msgs) == nil {
		t.Error("a 500 produced no fetchErrMsg")
	}
	if findAgentsMsg(msgs) != nil {
		t.Error("a failed fetch produced an AgentsMsg; panes must never see a zero-valued result")
	}
}

// TestFetchErrorLeavesPriorPaneDataIntact is the AC's error test, end to end
// through the real panes: poll successfully, then fail, and the frame must
// still show the fleet from the last good poll.
func TestFetchErrorLeavesPriorPaneDataIntact(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = sized.(model)

	// First poll succeeds and reaches the panes.
	for _, msg := range runTick(m) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	loaded := m.View()
	if !strings.Contains(loaded, "3 agents") {
		t.Fatalf("a successful poll did not reach the AGENTS pane:\n%s", loaded)
	}

	// Second poll fails.
	server.fail.Store(true)
	failed := runTick(m)
	if findFetchErr(failed) == nil {
		t.Fatal("the second poll did not fail as arranged")
	}
	for _, msg := range failed {
		next, _ := m.Update(msg)
		m = next.(model)
	}

	if got := m.View(); got != loaded {
		t.Errorf("a failed poll changed the frame; prior data must survive\nbefore:\n%s\nafter:\n%s", loaded, got)
	}
	if n := server.requests.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2 — the second tick must really re-fetch", n)
	}
}

// TestFetchErrMsgNeverReachesAPane pins the MECHANISM behind the test above.
//
// The previous test would also pass if the error reached the panes and every
// pane happened to ignore it — which is true of the stubs today and will stop
// being true as each pane grows. The contract is that the app swallows the
// error, so no pane ever has to decide what to do with one.
func TestFetchErrMsgNeverReachesAPane(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m, fakes := modelWithFakes()

	if _, cmd := m.Update(fetchErrMsg{source: "agents", err: http.ErrServerClosed}); cmd != nil {
		t.Error("a swallowed error produced a command")
	}
	for i, f := range fakes {
		if f.updates != 0 {
			t.Errorf("pane %d saw the fetch error; the app must swallow it", i)
		}
	}
}

// TestPollResultsBroadcastToEveryPane pins the other side of the routing
// contract: a poll result is not addressed to whichever pane happens to be
// focused, so every pane sees it and each decides whether the message is its
// own.
func TestPollResultsBroadcastToEveryPane(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m, fakes := modelWithFakes()
	m.focus = 2

	if _, cmd := m.Update(panes.AgentsMsg{}); cmd == nil {
		t.Error("the panes' commands were dropped")
	}
	for i, f := range fakes {
		if f.updates != 1 {
			t.Errorf("pane %d saw %d updates, want 1", i, f.updates)
		}
	}
}

// TestFetchErrMsgReportsSourceAndCause: the error is not displayed yet, so the
// only thing keeping it useful for the task that displays it is that it
// carries which fetch failed and why. A message that collapsed to "poll
// failed" would make that task start by re-plumbing this one.
func TestFetchErrMsgReportsSourceAndCause(t *testing.T) {
	err := fetchErrMsg{source: "agents", err: http.ErrServerClosed}
	got := err.Error()
	if !strings.Contains(got, "agents") {
		t.Errorf("Error() = %q, want it to name the failing fetch", got)
	}
	if !strings.Contains(got, http.ErrServerClosed.Error()) {
		t.Errorf("Error() = %q, want it to carry the underlying cause", got)
	}
}

// TestPollSurvivesAnUnreachableDashboard is the case an operator actually
// hits: the TUI started before the dashboard did. It must produce an error and
// a re-armed tick, not a panic and not a hang.
func TestPollSurvivesAnUnreachableDashboard(t *testing.T) {
	m := pollTestModel(t, closedDashboard)

	done := make(chan []tea.Msg, 1)
	go func() { done <- runTick(m) }()

	select {
	case msgs := <-done:
		if findFetchErr(msgs) == nil {
			t.Error("an unreachable dashboard produced no fetchErrMsg")
		}
		if !hasTick(msgs) {
			t.Error("an unreachable dashboard stopped the tick loop")
		}
	case <-time.After(finalWait):
		t.Fatal("a poll against an unreachable dashboard did not return")
	}
}

// ── T29: governor + header wiring ────────────────────────────────────────────

// governorStatusFixture is the /api/status body the T29 tests serve, trimmed
// to the keys client.GovernorStatus reads. The mode is deliberately lowercase,
// as buildGovernor sends it, so a test can tell a case-folding header from one
// that echoes the wire.
const governorStatusFixture = `{
  "governor": {
    "active": true, "mode": "surge", "issues": 12, "prs": 3,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:05 PM UTC"
  },
  "acmmLevel": 4,
  "acmmLevelConfigured": true
}`

// governorConfigFixture is the /api/config/governor body. Only the one nested
// key GovernorEvalInterval reads is present; the real response is far larger.
const governorConfigFixture = `{"general_advanced": {"eval_interval_s": 300}}`

// hiveIDFixture is the /api/hive-id body (T6b, #5412).
const hiveIDFixture = `{"id": "acme-prod"}`

// governorEvalInterval is what governorConfigFixture decodes to.
const governorEvalInterval = 300 * time.Second

// dashboardServer serves the four endpoints the poll reads, with a per-path
// failure switch.
//
// FAILING BY PATH IS THE WHOLE POINT. T29's core invariant is that these reads
// fail independently, and a server that could only be all-up or all-down could
// not express the case that matters: /api/status fine, /api/config/governor
// forbidden. Each path also counts its requests, so a test can prove a second
// tick re-read rather than replaying a cache.
type dashboardServer struct {
	*httptest.Server
	failStatus atomic.Bool
	failConfig atomic.Bool
	failHiveID atomic.Bool
	hiveID     atomic.Value // string body for /api/hive-id
}

func newDashboardServer(t *testing.T) *dashboardServer {
	t.Helper()
	s := &dashboardServer{}
	s.hiveID.Store(hiveIDFixture)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/agents":
			_, _ = w.Write([]byte(agentsFixture))
		case "/api/status":
			if s.failStatus.Load() {
				fail()
				return
			}
			_, _ = w.Write([]byte(governorStatusFixture))
		case "/api/config/governor":
			if s.failConfig.Load() {
				fail()
				return
			}
			_, _ = w.Write([]byte(governorConfigFixture))
		case "/api/hive-id":
			if s.failHiveID.Load() {
				fail()
				return
			}
			body, _ := s.hiveID.Load().(string)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// applyAll feeds every message a poll produced back into the model, the way
// bubbletea's runtime would, and returns the settled model.
//
// It exists because T29's behaviour is a two-stage pipeline — a fetch produces
// an app-level message, and Update turns cached app state into a pane message
// — so a test that only inspected the poll's output would be asserting on the
// wrong half.
func applyAll(m model, msgs []tea.Msg) model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

// pollAndApply runs one poll against the server and settles every result.
func pollAndApply(t *testing.T, m model) model {
	t.Helper()
	return applyAll(m, drain(m.poll()))
}

// deliveredGovernor is the frame the model would hand the panes, or nil when
// no successful status read has happened yet.
//
// It reads model state rather than intercepting a message because broadcast
// delivers INTO the panes and returns only their commands — the frame itself
// never appears in any Cmd's output. governorLoaded is the same distinction
// the app makes: no status read yet is not the same fact as a status read that
// reported an inactive governor.
func deliveredGovernor(m model) *panes.GovernorMsg {
	if !m.governorLoaded {
		return nil
	}
	msg := m.governorMsg()
	return &msg
}

// TestPollPopulatesGovernorAndHeaderWithoutSSE is the first acceptance
// criterion: startup polling alone fills the Governor pane and both header
// fields, with no stream event involved.
func TestPollPopulatesGovernorAndHeaderWithoutSSE(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	got := deliveredGovernor(settled)
	if got == nil {
		t.Fatal("a successful poll delivered no GovernorMsg; the pane would stay on its waiting placeholder")
	}
	if got.Status.Mode != "surge" {
		t.Errorf("GovernorMsg.Status.Mode = %q, want %q", got.Status.Mode, "surge")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("GovernorMsg.EvalInterval = %v, want %v", got.EvalInterval, governorEvalInterval)
	}

	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q", settled.headerText(), want)
	}
}

// TestGovernorMsgAlwaysCarriesTheCachedInterval is the bug this task exists to
// close, stated directly: a stream event carries no evaluation interval, so a
// GovernorMsg built from one alone reverts `next eval` to unknown.
func TestGovernorMsgAlwaysCarriesTheCachedInterval(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)
	m = pollAndApply(t, m)

	if m.governorInterval != governorEvalInterval {
		t.Fatalf("poll cached interval %v, want %v", m.governorInterval, governorEvalInterval)
	}

	// Now let the stream deliver a full status event, as it would a moment
	// later. Before T29 this is the message that blanked the interval.
	m = connectedStream(t, m)
	// The command is deliberately NOT drained: handleSSEEvent re-arms the
	// stream pump, and running that Cmd would block forever on a test stream
	// nothing writes to. The frame is read from the settled model instead.
	next, _ := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeMessage, statusFixture),
	})
	m = next.(model)

	got := deliveredGovernor(m)
	if got == nil {
		t.Fatal("a full SSE status event delivered no GovernorMsg")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("SSE-sourced GovernorMsg.EvalInterval = %v, want the cached %v; a zero here is the `next eval` regression",
			got.EvalInterval, governorEvalInterval)
	}
	// The stream's own mode must land too — that is the "updates immediately"
	// half of the same criterion.
	if got.Status.Mode != "busy" {
		t.Errorf("GovernorMsg.Status.Mode = %q, want the streamed %q", got.Status.Mode, "busy")
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("model interval = %v after an SSE event, want it retained", m.governorInterval)
	}

	// The pane-message builder itself must carry the interval too. This is the
	// exact call site that shipped `panes.GovernorMsg{Status: status}` with no
	// interval, so asserting on it directly is what stops the regression
	// reappearing there specifically.
	direct := findGovernorMsg(m.paneMsgs(sseEvent(client.SSEEventTypeMessage, statusFixture)))
	if direct == nil {
		t.Fatal("paneMsgs produced no governor frame for a full status event")
	}
	if direct.EvalInterval != governorEvalInterval {
		t.Errorf("paneMsgs GovernorMsg.EvalInterval = %v, want the cached %v", direct.EvalInterval, governorEvalInterval)
	}
}

// connectedStream puts m into the connected state with a live stream attached,
// so sseEventMsg is not dropped by the generation guard.
func connectedStream(t *testing.T, m model) model {
	t.Helper()
	stream := &sseStream{
		events: make(chan client.SSEEvent),
		errs:   make(chan error),
		cancel: func() {},
		gen:    m.sseGen,
	}
	m.sse = stream
	m.sseConnected = true
	return m
}

// TestAgentOnlySSEEventPreservesGovernorAndHeader pins that the light
// agent-status push, which carries no governor object, leaves the cached mode
// and the header alone rather than clearing them.
func TestAgentOnlySSEEventPreservesGovernorAndHeader(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)
	m = pollAndApply(t, m)
	before := m.headerText()

	m = connectedStream(t, m)
	next, _ := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeAgentStatus, agentStatusFixture),
	})
	got := next.(model)

	if got.governorStatus.Mode != "surge" {
		t.Errorf("governor mode = %q after an agent-only event, want the cached %q", got.governorStatus.Mode, "surge")
	}
	if got.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v after an agent-only event, want it retained", got.governorInterval)
	}
	// The header's ws field legitimately flips to connected; the data fields
	// must not move.
	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsConnected)
	if got.headerText() != want {
		t.Errorf("header = %q after an agent-only event, want %q (was %q)", got.headerText(), want, before)
	}
}

// TestForbiddenConfigReadKeepsLiveGovernorMode is the failure-isolation
// criterion in its most concrete form: a read-only token that cannot see
// /api/config/governor must still get a live mode in the header and a loaded
// Governor pane.
func TestForbiddenConfigReadKeepsLiveGovernorMode(t *testing.T) {
	server := newDashboardServer(t)
	server.failConfig.Store(true)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	got := deliveredGovernor(settled)
	if got == nil {
		t.Fatal("a forbidden config read suppressed the whole governor frame")
	}
	if got.Status.Mode != "surge" {
		t.Errorf("Status.Mode = %q, want the live %q despite the config failure", got.Status.Mode, "surge")
	}
	// The interval is honestly unknown, which the pane renders as a dash.
	if got.EvalInterval != 0 {
		t.Errorf("EvalInterval = %v, want zero when the config read failed", got.EvalInterval)
	}
	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q; a config failure must not blank the mode", settled.headerText(), want)
	}
}

// TestFailedStatusReadKeepsHiveIdentity is the mirror case: /api/status down
// must not take the header's identity with it.
func TestFailedStatusReadKeepsHiveIdentity(t *testing.T) {
	server := newDashboardServer(t)
	server.failStatus.Store(true)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) != nil {
		t.Error("a failed status read still delivered a governor frame; the pane would show invented values")
	}
	want := fmt.Sprintf(headerFormat, "acme-prod", headerUnknown, wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q; the identity read succeeded", settled.headerText(), want)
	}
}

// TestEmptyHiveIDDoesNotBlockTheGovernorPane pins the other half of the
// issue's isolation clause: an unnamed hive renders an honest dash and the
// Governor pane still loads.
func TestEmptyHiveIDDoesNotBlockTheGovernorPane(t *testing.T) {
	server := newDashboardServer(t)
	server.hiveID.Store(`{"id": ""}`)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) == nil {
		t.Fatal("an empty hive id stopped the Governor pane loading")
	}
	want := fmt.Sprintf(headerFormat, headerUnknown, "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q for a hive with no configured identity", settled.headerText(), want)
	}
}

// TestInactiveGovernorRendersADash pins that an inactive governor is a dash in
// the header rather than an empty or invented mode.
func TestInactiveGovernorRendersADash(t *testing.T) {
	m := newModel()
	m.hiveID = "acme-prod"
	m.governorStatus = client.GovernorStatus{
		GovernorState: client.GovernorState{Active: false, Mode: "busy"},
	}

	want := fmt.Sprintf(headerFormat, "acme-prod", headerUnknown, wsNotConnected)
	if m.headerText() != want {
		t.Errorf("header = %q, want %q; an inactive governor has no mode to report", m.headerText(), want)
	}
}

// TestMalformedResponsesRenderDashes pins that a body the client cannot decode
// is a failed read — dashes — rather than a zero value rendered as fact.
func TestMalformedResponsesRenderDashes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"this": "is not the shape you asked for"`))
	}))
	t.Cleanup(server.Close)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) != nil {
		t.Error("a malformed status body still produced a governor frame")
	}
	want := fmt.Sprintf(headerFormat, headerUnknown, headerUnknown, wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want all dashes for undecodable responses", settled.headerText())
	}
}

// TestHeaderSurvivesStreamDropAndRecovers walks the four states the AC asks to
// be pinned — startup, connected, degraded, recovered — as one sequence,
// because the property under test is precisely that the data fields do NOT
// move while the ws field does.
func TestHeaderSurvivesStreamDropAndRecovers(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	// 1. Startup: nothing fetched yet. Every field honest about that.
	startup := fmt.Sprintf(headerFormat, headerUnknown, headerUnknown, wsNotConnected)
	if m.headerText() != startup {
		t.Errorf("startup header = %q, want %q", m.headerText(), startup)
	}

	// 2. Connected: the poll has data and the stream is up.
	m = pollAndApply(t, m)
	m = connectedStream(t, m)
	connected := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsConnected)
	if m.headerText() != connected {
		t.Errorf("connected header = %q, want %q", m.headerText(), connected)
	}

	// 3. Degraded: the stream drops. ONLY ws changes — this is the "do not
	// treat an SSE connection as the data value" clause. A header that
	// derived identity or mode from the connection blanks them here.
	next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	m = next.(model)
	degraded := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if m.headerText() != degraded {
		t.Errorf("degraded header = %q, want %q; identity and mode must survive a stream drop", m.headerText(), degraded)
	}

	// 4. Recovered: the fallback poll keeps refreshing while the stream is
	// gone. The server now reports a different mode, and the header must
	// follow it without any stream event.
	if m.interval != pollInterval {
		t.Errorf("interval = %v after a drop, want the fallback cadence %v", m.interval, pollInterval)
	}
	m = pollAndApply(t, m)
	recovered := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if m.headerText() != recovered {
		t.Errorf("recovered header = %q, want %q", m.headerText(), recovered)
	}
}

// TestPollFallbackKeepsRefreshingGovernorAfterADrop is the fourth acceptance
// criterion. It changes what the server returns between ticks, so a model that
// merely retained its cache — rather than re-reading — fails.
func TestPollFallbackKeepsRefreshingGovernorAfterADrop(t *testing.T) {
	mode := atomic.Value{}
	mode.Store("quiet")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			current, _ := mode.Load().(string)
			_, _ = fmt.Fprintf(w, `{"governor":{"active":true,"mode":%q,"issues":1,"prs":0},
			  "acmmLevel":2,"acmmLevelConfigured":true}`, current)
		case "/api/config/governor":
			_, _ = w.Write([]byte(governorConfigFixture))
		case "/api/hive-id":
			_, _ = w.Write([]byte(hiveIDFixture))
		default:
			_, _ = w.Write([]byte(agentsFixture))
		}
	}))
	t.Cleanup(server.Close)

	m := pollTestModel(t, server.URL)
	m = connectedStream(t, m)
	m = pollAndApply(t, m)
	if m.governorStatus.Mode != "quiet" {
		t.Fatalf("mode = %q before the drop, want %q", m.governorStatus.Mode, "quiet")
	}

	next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	m = next.(model)

	// The hive gets busier while there is no stream to say so.
	mode.Store("surge")
	m = pollAndApply(t, m)

	if m.governorStatus.Mode != "surge" {
		t.Errorf("mode = %q after a fallback poll, want %q; the fallback stopped refreshing the governor",
			m.governorStatus.Mode, "surge")
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v after a fallback poll, want %v", m.governorInterval, governorEvalInterval)
	}
}

// TestPollIssuesAllFourReads pins that the batch actually contains every fetch
// T29 added. A regression that dropped one would otherwise only show up as a
// field that quietly never updates.
func TestPollIssuesAllFourReads(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.poll())

	var agents, governor, interval, hiveID bool
	for _, msg := range msgs {
		switch msg.(type) {
		case panes.AgentsMsg:
			agents = true
		case governorStatusMsg:
			governor = true
		case governorIntervalMsg:
			interval = true
		case hiveIDMsg:
			hiveID = true
		}
	}
	if !agents {
		t.Error("poll did not fetch agents")
	}
	if !governor {
		t.Error("poll did not fetch governor status")
	}
	if !interval {
		t.Error("poll did not fetch the governor eval interval")
	}
	if !hiveID {
		t.Error("poll did not fetch the hive id")
	}
}

// TestIntervalBeforeStatusDeliversNoFrame pins the ordering guard: config can
// answer before live state does, and a GovernorMsg then would carry a zero
// status the pane cannot tell from an inactive governor.
func TestIntervalBeforeStatusDeliversNoFrame(t *testing.T) {
	m := newModel()

	next, _ := m.Update(governorIntervalMsg{interval: governorEvalInterval})
	m = next.(model)

	if got := deliveredGovernor(m); got != nil {
		t.Errorf("an interval arriving before any status delivered a frame with status %+v", got.Status)
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v, want it cached for the first status read", m.governorInterval)
	}

	// The first status read then delivers both halves at once.
	next, _ = m.Update(governorStatusMsg{status: client.GovernorStatus{
		GovernorState: client.GovernorState{Active: true, Mode: "busy"},
	}})
	m = next.(model)
	got := deliveredGovernor(m)
	if got == nil {
		t.Fatal("the first status read delivered no frame")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("EvalInterval = %v, want the cached %v", got.EvalInterval, governorEvalInterval)
	}
}
