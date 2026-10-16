package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/github"
)

func TestClaimHooksRemoveLabelAfterSecondClaimExpires(t *testing.T) {
	var mu sync.Mutex
	present, adds, removes := false, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			labels := []map[string]string{}
			if present {
				labels = append(labels, map[string]string{"name": "claimed"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 63, "labels": labels})
		case http.MethodPost:
			adds++
			present = true
			_, _ = w.Write([]byte(`[{"name":"claimed"}]`))
		case http.MethodDelete:
			removes++
			present = false
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := github.NewClientForTest(srv.URL, "o", nil, logger)
	dir := t.TempDir()
	ml, err := mutation.OpenLedger(filepath.Join(dir, "mutations.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetMutationBoundary(&mutation.Boundary{Executor: mutation.Executor{Ledger: ml, Journal: journal, Mode: "enforce"}, Holder: "test"})
	cfg := &config.Config{}
	no := false
	cfg.Governor.Claims.Comment = &no
	hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return c }, logger)
	l, err := claims.New(filepath.Join(dir, "claims.json"), claims.DefaultPolicy(), hooks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.SetNow(func() time.Time { return now })
	wait := func(wantAdds, wantRemoves int) {
		t.Helper()
		key := mutation.TaskClaim("o/r", "o/r#label_mutation/63").Key()
		testutil.Eventually(t, 5*time.Second, func() bool {
			mu.Lock()
			a, r := adds, removes
			mu.Unlock()
			if a != wantAdds || r != wantRemoves {
				return false
			}
			// Wait until the boundary has durably acknowledged the HTTP response
			// and released the writer before starting the next hook.
			entry, ok := ml.Get(key)
			return ok && entry.State == mutation.StateReleased
		}, "hooks did not complete: want adds=%d removes=%d", wantAdds, wantRemoves)
	}
	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := l.Claim(claims.Request{Repo: "o/r", Issue: 63, Holder: "alice", Kind: claims.KindHuman, TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
		wait(cycle, cycle-1)
		now = now.Add(2 * time.Minute)
		if n := l.Expire(); n != 1 {
			t.Fatalf("expired %d claims, want 1", n)
		}
		wait(cycle, cycle)
	}
	mu.Lock()
	defer mu.Unlock()
	if present {
		t.Fatal("claimed label remains after second expiry")
	}
}
