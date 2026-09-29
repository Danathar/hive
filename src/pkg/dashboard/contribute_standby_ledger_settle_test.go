package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func TestStandbySuspensionReconcilesClosedDonatedPRs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/", func(w http.ResponseWriter, r *http.Request) {
		num := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/")
		n, _ := strconv.Atoi(num)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": n,
			"state":  "closed",
			"merged": false,
			"user":   map[string]any{"login": "alice"},
			"base": map[string]any{
				"repo": map[string]any{
					"name":      "widgets",
					"full_name": "acme/widgets",
					"owner":     map[string]any{"login": "acme"},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	s := NewServer(0, logger)
	s.deps = &Dependencies{
		Ctx:      context.Background(),
		GHClient: ghpkg.NewClientForTest(ts.URL, "acme", []string{"widgets"}, logger),
	}
	h := NewContributeWSHub(logger, s)
	t.Cleanup(h.Close)
	h.standbyOutcomesFile = filepath.Join(t.TempDir(), standbyOutcomesFileName)
	key := standbyOutcomeKey("alice", standbypkg.Configuration{Backend: "copilot", Model: "gpt-5.4-mini"})
	h.standbyOutcomes = []standbyOutcomeRecord{
		{Key: key, Lane: "quality", Repo: "acme/widgets", Number: 41, Kind: standbypkg.OutcomeOpen, DispatchedAt: time.Now().Add(-time.Hour)},
		{Key: key, Lane: "quality", Repo: "acme/widgets", Number: 42, Kind: standbypkg.OutcomeOpen, DispatchedAt: time.Now().Add(-time.Hour)},
	}

	h.reconcileOpenStandbyOutcomes(context.Background())
	suspended, streak := h.standbySuspended(key)
	if !suspended || streak != 2 {
		t.Fatalf("standbySuspended = %v, %d; want suspended after two reconciled closed PRs", suspended, streak)
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	closed := 0
	for _, rec := range h.standbyOutcomes {
		if rec.Kind == standbypkg.OutcomeClosedUnmerged {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("closed_unmerged records = %d, want 2; ledger=%+v", closed, h.standbyOutcomes)
	}
}

func TestStandbyReadsDoNotReconcileAndMissingRowsRetire(t *testing.T) {
	for _, status := range []int{404, 403, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
			}))
			defer ts.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := NewServer(0, logger)
			s.deps = &Dependencies{GHClient: ghpkg.NewClientForTest(ts.URL, "acme", []string{"widgets"}, logger)}
			h := NewContributeWSHub(logger, s)
			defer h.Close()
			key := standbyOutcomeKey("alice", standbypkg.Configuration{})
			for n := 1; n <= 10; n++ {
				h.standbyOutcomes = append(h.standbyOutcomes, standbyOutcomeRecord{Key: key, Lane: "quality", Repo: "acme/widgets", Number: n, Kind: standbypkg.OutcomeOpen})
			}
			for i := 0; i < 5; i++ {
				h.mu.RLock()
				suspended, streak := h.standbySuspended(key)
				h.mu.RUnlock()
				if suspended || streak != 0 {
					t.Fatalf("unknown PRs suspended donor: %v %d", suspended, streak)
				}
				if h.SuspendedStandbyCounts([]string{"quality"})["quality"] != 0 {
					t.Fatal("counts disagree")
				}
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("hot-path GETs = %d, want 0", got)
			}
			h.reconcileOpenStandbyOutcomes(context.Background())
			h.reconcileOpenStandbyOutcomes(context.Background())
			want := int32(20)
			if status == 404 {
				want = 10
			}
			if got := requests.Load(); got != want {
				t.Fatalf("background GETs = %d, want %d", got, want)
			}
			if suspended, streak := h.standbySuspended(key); suspended || streak != 0 {
				t.Fatalf("failed lookups penalized donor: %v %d", suspended, streak)
			}
		})
	}
}

func TestStandbyReconcileCancellationStopsRequests(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(0, logger)
	s.deps = &Dependencies{GHClient: ghpkg.NewClientForTest(ts.URL, "acme", []string{"widgets"}, logger)}
	h := NewContributeWSHub(logger, s)
	defer h.Close()
	h.standbyOutcomes = []standbyOutcomeRecord{{Key: "alice|", Repo: "acme/widgets", Number: 1, Kind: standbypkg.OutcomeOpen}, {Key: "alice|", Repo: "acme/widgets", Number: 2, Kind: standbypkg.OutcomeOpen}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.reconcileOpenStandbyOutcomes(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not stop")
	}
}
