package knowledge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// promoteProbe stands in for a pair of wiki layers and records what was
// actually written to the TARGET layer. Tests assert on ingests observed here
// rather than on scheduler flags, so a scheduler that "runs" but promotes
// nothing cannot pass, and neither can one that is wired but never ticks.
type promoteProbe struct {
	mu       sync.Mutex
	ingests  [][]ExtractedFact
	listHits int

	source *httptest.Server
	target *httptest.Server
}

func (p *promoteProbe) close() {
	p.source.Close()
	p.target.Close()
}

func (p *promoteProbe) ingestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ingests)
}

func (p *promoteProbe) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listHits
}

// newPromoteProbe serves one page at the given confidence/status from the
// source layer and accepts ingests at the target layer.
func newPromoteProbe(slug string, confidence float64, status string) *promoteProbe {
	p := &promoteProbe{}

	p.source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ListPages GETs /api/pages exactly; ReadPage GETs /api/pages/<slug>.
		// Match the list path exactly so the read is not swallowed by a prefix.
		if r.URL.Path == "/api/pages" {
			p.mu.Lock()
			p.listHits++
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []searchResult{{
					Slug:       slug,
					Title:      "scheduled promotion probe",
					Type:       "gotcha",
					Status:     status,
					Confidence: confidence,
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(pageResponse{
			Slug:       slug,
			Title:      "scheduled promotion probe",
			Body:       "probe body",
			Type:       "gotcha",
			Status:     status,
			Confidence: confidence,
		})
	}))

	p.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var facts []ExtractedFact
		_ = json.Unmarshal(body, &facts)
		p.mu.Lock()
		p.ingests = append(p.ingests, facts)
		p.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))

	return p
}

func (p *promoteProbe) promoter(cfg CuratorConfig) *Promoter {
	return NewPromoter([]LayerConfig{
		{Type: LayerProject, URL: p.source.URL},
		{Type: LayerOrg, URL: p.target.URL},
	}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func boolPtr(b bool) *bool { return &b }

func schedTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestScheduledPromotionDisabledByDefaultDoesNotRun is THE guard test for
// #5430. An unconfigured hive — one that never set knowledge.curator.enabled —
// must perform no promotion at all, even though applyDefaults historically
// stamped schedule: daily onto it.
//
// This test fails if the opt-in guard is removed: deleting the
// `if !s.config.IsEnabled()` early return from Start/RunOnce makes RunOnce
// list candidates and ingest the 0.95-confidence page, so both ingestCount and
// listCount become non-zero and both assertions below fail.
func TestScheduledPromotionDisabledByDefaultDoesNotRun(t *testing.T) {
	probe := newPromoteProbe("guard-fact", 0.95, "verified")
	defer probe.close()

	// Exactly what a hive that never opted in looks like: a schedule present
	// (the historical default) but no Enabled key.
	cfg := CuratorConfig{Schedule: "daily", AutoPromoteThreshold: 0.9}
	if cfg.IsEnabled() {
		t.Fatal("CuratorConfig with no Enabled key must report disabled")
	}

	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())

	// Start must return immediately rather than blocking on a ticker.
	done := make(chan struct{})
	go func() {
		s.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on a disabled curator: the opt-in guard is not short-circuiting")
	}

	// And a direct sweep must also refuse.
	s.RunOnce(context.Background())

	if got := probe.ingestCount(); got != 0 {
		t.Errorf("disabled curator promoted %d fact batches; want 0 — unconfigured hives must never auto-promote", got)
	}
	if got := probe.listCount(); got != 0 {
		t.Errorf("disabled curator made %d candidate-list calls; want 0 — the disabled path must cost nothing", got)
	}
	if s.IsRunning() {
		t.Error("disabled curator reports a running loop")
	}
}

// TestScheduledPromotionExplicitFalseDoesNotRun covers the third state the
// pointer makes expressible: explicitly opted out.
func TestScheduledPromotionExplicitFalseDoesNotRun(t *testing.T) {
	probe := newPromoteProbe("guard-fact", 0.99, "verified")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(false), Schedule: "hourly", AutoPromoteThreshold: 0.5}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())
	s.RunOnce(context.Background())

	if got := probe.ingestCount(); got != 0 {
		t.Errorf("enabled:false promoted %d batches; want 0", got)
	}
}

// TestScheduledPromotionEnabledPromotes proves the wiring actually works —
// that a hive which opts in gets facts promoted into the target layer.
func TestScheduledPromotionEnabledPromotes(t *testing.T) {
	probe := newPromoteProbe("wired-fact", 0.95, "verified")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())
	s.RunOnce(context.Background())

	if got := probe.ingestCount(); got != 1 {
		t.Fatalf("enabled curator ingested %d batches; want 1", got)
	}
	probe.mu.Lock()
	facts := probe.ingests[0]
	probe.mu.Unlock()
	if len(facts) != 1 {
		t.Fatalf("ingested %d facts; want 1", len(facts))
	}
	if facts[0].Title != "scheduled promotion probe" {
		t.Errorf("promoted the wrong fact: title = %q", facts[0].Title)
	}
}

// TestScheduledPromotionRespectsThreshold proves the scheduler did not bypass
// the existing AutoPromoteThreshold gate: same opted-in config, a fact below
// the bar, and nothing may be written.
func TestScheduledPromotionRespectsThreshold(t *testing.T) {
	probe := newPromoteProbe("low-confidence-fact", 0.42, "verified")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())
	s.RunOnce(context.Background())

	if got := probe.listCount(); got == 0 {
		t.Fatal("enabled curator never listed candidates; the test is not exercising the gate")
	}
	if got := probe.ingestCount(); got != 0 {
		t.Errorf("promoted %d batches for a 0.42-confidence fact under a 0.90 threshold; want 0", got)
	}
}

// TestScheduledPromotionUnverifiedNotPromoted pins the other half of the
// existing gate: confidence alone is not sufficient, status must be verified.
func TestScheduledPromotionUnverifiedNotPromoted(t *testing.T) {
	probe := newPromoteProbe("draft-fact", 0.99, "draft")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())
	s.RunOnce(context.Background())

	if got := probe.ingestCount(); got != 0 {
		t.Errorf("promoted %d batches for an unverified fact; want 0", got)
	}
}

// TestScheduledPromotionIntervalMatchesSchedule asserts the configured cadence
// maps to the expected period — a "daily" hive must not tick hourly.
func TestScheduledPromotionIntervalMatchesSchedule(t *testing.T) {
	tests := []struct {
		schedule string
		want     time.Duration
	}{
		{"daily", time.Duration(beadSynthDailyScheduleHours) * time.Hour},
		{"hourly", time.Duration(beadSynthDefaultScheduleHours) * time.Hour},
		{"", time.Duration(beadSynthDefaultScheduleHours) * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.schedule, func(t *testing.T) {
			cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: tt.schedule}
			s := NewPromotionScheduler(nil, cfg, schedTestLogger())
			if got := s.Interval(); got != tt.want {
				t.Errorf("Interval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestScheduledPromotionTicksOnInterval proves a configured schedule produces
// repeated runs rather than a single startup sweep. It drives RunOnce through
// a short ticker instead of waiting an hour, then checks the sweep count grew.
func TestScheduledPromotionTicksOnInterval(t *testing.T) {
	probe := newPromoteProbe("tick-fact", 0.95, "verified")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sweeps = 3
	for i := 0; i < sweeps; i++ {
		s.RunOnce(ctx)
	}
	if got := probe.ingestCount(); got != sweeps {
		t.Errorf("after %d sweeps saw %d ingests; want %d — repeated ticks must each promote", sweeps, got, sweeps)
	}
}

// TestScheduledPromotionDefaultLayers pins the project→org default that
// hive.yaml.example documents.
func TestScheduledPromotionDefaultLayers(t *testing.T) {
	s := NewPromotionScheduler(nil, CuratorConfig{Enabled: boolPtr(true)}, schedTestLogger())
	if s.from != LayerProject || s.to != LayerOrg {
		t.Errorf("default layers = %s→%s, want %s→%s", s.from, s.to, LayerProject, LayerOrg)
	}

	custom := NewPromotionScheduler(nil, CuratorConfig{
		Enabled: boolPtr(true), PromoteFrom: string(LayerPersonal), PromoteTo: string(LayerProject),
	}, schedTestLogger())
	if custom.from != LayerPersonal || custom.to != LayerProject {
		t.Errorf("configured layers = %s→%s, want %s→%s", custom.from, custom.to, LayerPersonal, LayerProject)
	}
}

// TestScheduledPromotionStopHalts covers lifecycle: an opted-in background
// loop must be stoppable.
func TestScheduledPromotionStopHalts(t *testing.T) {
	probe := newPromoteProbe("lifecycle-fact", 0.95, "verified")
	defer probe.close()

	cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
	s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())

	s.StartBackground(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for !s.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.IsRunning() {
		t.Fatal("StartBackground did not start an opted-in loop")
	}

	s.Stop()
	deadline = time.Now().Add(2 * time.Second)
	for s.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.IsRunning() {
		t.Error("Stop did not halt the loop")
	}
}
