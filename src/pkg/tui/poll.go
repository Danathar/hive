package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/client"
	"github.com/kubestellar/hive/pkg/tui/panes"
)

// pollInterval is how often the TUI re-reads the dashboard API.
//
// 5s is not a taste call: it is the cadence the dashboard already refreshes
// at. `dashboard/server.js:111` sets `REFRESH_MS = 5000` and drives its SSE
// push loop from it (`server.js:803`), so 5s is the freshest any consumer of
// this API ever sees — polling faster would re-fetch a snapshot the server has
// not rebuilt, spending request budget for no new information. It also matches
// the client's own 5s request timeout (pkg/tui/client/client.go), which was
// chosen on the same reasoning: a request that outlives its frame is not worth
// waiting for.
//
// T13b replaces this loop with the SSE stream and keeps it as the fallback;
// having picked the stream's own cadence means that switch changes where the
// data comes from without changing how often the frame moves. It is also the
// cadence the fallback returns to: while the stream is up the loop stretches to
// sseReconcileInterval (app.go), and a dropped stream puts it back here.
const pollInterval = 5 * time.Second

// tickMsg is the poll heartbeat. It carries no time: the tick is a "go fetch
// now" signal, not a clock, and nothing reads when it fired. tea.Tick supplies
// the instant to the callback, which discards it — T13b made this a struct for
// the generation below, and a field kept only because the callback is handed
// one would be a value no reader could rely on.
//
// GEN IS WHAT KEEPS THE LOOP SINGLE. tea.Tick fires once and the loop stays
// alive by re-arming from the handler, so "arm the new cadence" and "the old
// cadence is still armed" are the same instant: T13b changes the cadence when
// the SSE stream connects or drops, and arming a replacement chain without
// retiring the old one would leave two live chains ticking forever — one
// cadence change doubling the fetch rate for the rest of the process's life.
// Each chain therefore carries the model's tick generation, and a tick whose
// generation no longer matches is dropped instead of re-armed. That is what
// ends the superseded chain at its next fire rather than running it alongside
// the new one.
type tickMsg struct {
	gen uint64
}

// fetchErrMsg reports that one poll failed.
//
// It never reaches a pane. The app swallows it, which is the whole error
// policy: panes only ever see successful data, so the previous data survives a
// failed fetch by construction rather than by every pane remembering to hold
// onto it. The loop is unaffected — the next tick was already armed before the
// fetch was issued, so a dashboard that is down simply produces a stale frame
// that catches up when it returns.
//
// Nothing displays it yet, and that is a deliberate gap rather than an
// oversight: an error line is UI, and inventing one here would render into the
// frame T3 pinned and the header T13b owns. Carrying the source and the cause
// (rather than discarding them at the point of failure) is what lets that
// later task surface the real message instead of a generic "poll failed".
type fetchErrMsg struct {
	// source names the fetch that failed, so a frame with several polls in
	// flight can say which pane is stale rather than that something is.
	source string
	err    error
}

// Error makes fetchErrMsg an ordinary error value: the task that adds an
// error line can print it verbatim, and a test can assert on the text without
// this type needing accessors.
func (e fetchErrMsg) Error() string {
	return fmt.Sprintf("%s poll failed: %v", e.source, e.err)
}

// scheduleTick arms the next heartbeat.
//
// tea.Tick fires ONCE, so the loop is kept alive by re-arming from the tickMsg
// handler rather than by a repeating timer. That is not merely how bubbletea
// spells it — it is what stops ticks stacking: the next one is scheduled
// relative to the moment this one was handled, so a slow dashboard cannot
// queue up a backlog of pending fetches that all land at once when it
// recovers.
func (m model) scheduleTick() tea.Cmd {
	gen := m.tickGen
	return tea.Tick(m.interval, func(time.Time) tea.Msg {
		return tickMsg{gen: gen}
	})
}

// poll issues every fetch the client can currently make, as one batch.
//
// Four reads today: /api/agents (T4, #5067), plus the three T29 wired for the
// Governor pane and the header — /api/status for live governor state,
// /api/config/governor for the evaluation cadence, and /api/hive-id for the
// hive's identity. The tokens and events fetches (T8/T10) each add one line
// here and one message type in pkg/tui/panes; the loop, the error policy and
// the tick scheduling do not change when they land.
//
// EACH FETCH FAILS ALONE. They are separate Cmds in one batch rather than one
// Cmd making four calls, and that is the failure-isolation property T29 is
// about: a dashboard that serves /api/status but forbids /api/config/governor
// (a common read-only-token shape) must still show a live governor mode, and a
// hive with no configured identity must not stop the Governor pane loading.
// Folding them together would make every value only as available as the least
// available endpoint, because one error return would discard three good
// results. Batched Cmds also run concurrently, so four reads cost one round
// trip of wall time, not four.
//
// Deliberately NOT polled: /api/health. It exists and would succeed, but
// nothing in the frame renders it — the header's `ws:` field is SSE connection
// state, which is T13b's, not API reachability. Polling an endpoint whose
// result cannot be displayed would spend a request every 5s to learn nothing.
func (m model) poll() tea.Cmd {
	return tea.Batch(
		m.fetchAgents(),
		m.fetchGovernor(),
		m.fetchGovernorInterval(),
		m.fetchHiveID(),
	)
}

// fetchAgents resolves to a panes.AgentsMsg on success and a fetchErrMsg on
// failure — never to a partial or zero-valued AgentsMsg, which a pane would
// be unable to tell from an empty fleet.
//
// The request is bounded by the client's own 5s timeout rather than by a
// context deadline set here. A second, shorter deadline would silently
// override the one pkg/tui/client documents and make the effective timeout
// depend on which caller you read.
func (m model) fetchAgents() tea.Cmd {
	return func() tea.Msg {
		agents, err := m.api.Agents(context.Background())
		if err != nil {
			return fetchErrMsg{source: "agents", err: err}
		}
		return panes.AgentsMsg{Agents: agents}
	}
}

// governorStatusMsg is a successful live-governor read from GET /api/status.
//
// It is an APP-LEVEL message, not panes.GovernorMsg, and the indirection is
// the fix for the bug T29 exists to close. The pane's message must carry both
// the live status and the configured evaluation interval, but those come from
// two endpoints that fail independently and answer at different times. Sending
// panes.GovernorMsg straight from this fetch would mean sending it with
// whatever interval this Cmd happened to know — zero — which is precisely how
// the pre-T29 SSE path left `next eval` permanently unknown. Instead the app
// caches this, joins it with the last successful interval, and emits one
// GovernorMsg that is always complete. The header reads the same cache for its
// `governor:` field.
type governorStatusMsg struct {
	status client.GovernorStatus
}

// governorIntervalMsg is a successful configuration read from
// GET /api/config/governor.
//
// A zero duration is a legitimate answer — the hive has no evaluation interval
// configured — and is retained as such rather than treated as a miss, because
// the pane renders zero as an honest dash. What must never reach the model is
// the zero produced by a FAILED read, which is why failure travels as
// fetchErrMsg and never as this type carrying a default value.
type governorIntervalMsg struct {
	interval time.Duration
}

// hiveIDMsg is a successful identity read from GET /api/hive-id.
//
// An empty id is a valid answer, kept for the same reason a zero interval is:
// a hive with no configured name renders `hive: —`, and that dash is a fact
// the server reported rather than a fetch that failed. The distinction is
// carried by the type — a failure is a fetchErrMsg — so the header can hold
// the last good identity through an outage instead of blanking on it.
type hiveIDMsg struct {
	id string
}

// fetchGovernor reads the governor's live state.
//
// Live state and configuration are separate fetches on purpose; client.Governor
// documents the same split from the other side. The consequence worth naming
// here is the failure one: this call is the only source of the header's
// governor mode, so it must not be able to fail because a DIFFERENT endpoint
// did.
func (m model) fetchGovernor() tea.Cmd {
	return func() tea.Msg {
		status, err := m.api.Governor(context.Background())
		if err != nil {
			return fetchErrMsg{source: "governor", err: err}
		}
		return governorStatusMsg{status: status}
	}
}

// fetchGovernorInterval reads the governor's configured evaluation cadence.
//
// This is configuration, and client.GovernorEvalInterval notes it is worth
// fetching once rather than every tick. It is nonetheless polled on the normal
// cadence, because the alternative — fetch once at startup — makes the value
// permanently unknown for any TUI that started while the dashboard was down or
// while its token lacked config read access, with no path to recovery short of
// restarting. Re-reading it costs one small request per tick and is what lets
// `next eval` start working the moment access is restored.
func (m model) fetchGovernorInterval() tea.Cmd {
	return func() tea.Msg {
		interval, err := m.api.GovernorEvalInterval(context.Background())
		if err != nil {
			return fetchErrMsg{source: "governor config", err: err}
		}
		return governorIntervalMsg{interval: interval}
	}
}

// fetchHiveID reads the hive's display identity for the header.
//
// It is polled rather than fetched once for the recovery reason above, and
// because identity is cheap: hiveIDResponse is a single string off a dedicated
// endpoint (T6b, #5412) rather than a slice of the large status document.
func (m model) fetchHiveID() tea.Cmd {
	return func() tea.Msg {
		id, err := m.api.HiveID(context.Background())
		if err != nil {
			return fetchErrMsg{source: "hive id", err: err}
		}
		return hiveIDMsg{id: id}
	}
}
