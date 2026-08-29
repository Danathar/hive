package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGovernorDecodesFixture decodes testdata/governor.json — an excerpt of a
// real /api/status document, siblings included — and asserts every field of
// GovernorStatus.
//
// The sibling keys (timestamp, statusSeq, budget, acmmPackAgents, …) are not
// decoration: they are what proves this is a projection. /api/status is a large
// payload and this type models a few fields of it, so the test has to show that
// the keys it does not model are ignored rather than causing a decode failure.
func TestGovernorDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "governor.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Governor(context.Background())
	if err != nil {
		t.Fatalf("Governor() = %v, want nil", err)
	}

	if gotPath != "/api/status" {
		t.Errorf("path = %q, want /api/status", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}

	want := GovernorStatus{
		GovernorState: GovernorState{
			Active:     true,
			Mode:       "busy",
			Issues:     9,
			PRs:        3,
			Thresholds: GovernorThresholds{Quiet: 2, Busy: 10, Surge: 20},
			NextKick:   "8/29 3:09 PM PDT",
		},
		ACMMLevel:           4,
		ACMMLevelConfigured: true,
	}
	if got != want {
		t.Errorf("Governor() = %+v, want %+v", got, want)
	}

	// The ACMM fields are the ones the embedding could plausibly get wrong:
	// they live at the TOP level of the payload, not inside `governor`, so a
	// struct that nested them would decode them as zero and this comparison
	// would be the only thing to notice.
	if got.ACMMLevel != 4 || !got.ACMMLevelConfigured {
		t.Errorf("ACMM = (%d, %t), want (4, true) — top-level fields, not nested under governor",
			got.ACMMLevel, got.ACMMLevelConfigured)
	}

	if got.QueueDepth() != 12 {
		t.Errorf("QueueDepth() = %d, want 12 (9 issues + 3 PRs)", got.QueueDepth())
	}
}

// TestGovernorIdleHiveZeroValues covers the shape a brand-new hive sends: idle
// mode, nothing queued, no evaluation interval configured (so buildGovernor
// leaves nextKick empty and it is omitted from the wire), and an ACMM level of
// 1 that nobody chose.
//
// acmmLevelConfigured is the whole point of the case. detectACMMLevel returns 1
// for an unconfigured hive, exactly as it does for a hive explicitly set to L1,
// so a pane that renders "L1" from ACMMLevel alone cannot tell a default from a
// decision — this pins that the flag distinguishing them survives the decode.
func TestGovernorIdleHiveZeroValues(t *testing.T) {
	const body = `{
	  "governor": {
	    "active": true,
	    "mode": "idle",
	    "issues": 0,
	    "prs": 0,
	    "thresholds": {"quiet": 2, "busy": 10, "surge": 20}
	  },
	  "acmmLevel": 1,
	  "acmmLevelConfigured": false
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Governor(context.Background())
	if err != nil {
		t.Fatalf("Governor() = %v, want nil", err)
	}

	if got.Mode != "idle" {
		t.Errorf("Mode = %q, want idle", got.Mode)
	}
	if got.NextKick != "" {
		t.Errorf("NextKick = %q, want empty when the key is absent", got.NextKick)
	}
	if got.QueueDepth() != 0 {
		t.Errorf("QueueDepth() = %d, want 0", got.QueueDepth())
	}
	if got.ACMMLevel != 1 {
		t.Errorf("ACMMLevel = %d, want 1", got.ACMMLevel)
	}
	if got.ACMMLevelConfigured {
		t.Error("ACMMLevelConfigured = true, want false — an unset level reports 1 too, and only this flag separates them")
	}
}

// TestGovernorMalformedBody: a 200 carrying something that is not a JSON object
// is a decode error, not a silent zero-valued governor. A blank pane and a
// genuinely idle governor look identical to an operator, so this failing loudly
// is what keeps them distinguishable.
func TestGovernorMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Governor(context.Background())
	if err == nil {
		t.Fatal("Governor() = nil error on a non-JSON 200, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
	if got != (GovernorStatus{}) {
		t.Errorf("Governor() = %+v, want the zero value on error", got)
	}
}

// TestGovernorNonOKReturnsAPIError is the 500 error path: a typed *APIError
// carrying the status and the path, and a zero GovernorStatus.
func TestGovernorNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"status build failed"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Governor(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Governor() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Path != "/api/status" {
		t.Errorf("Path = %q, want /api/status", apiErr.Path)
	}
	if !strings.Contains(apiErr.Body, "status build failed") {
		t.Errorf("Body = %q, want it to quote the dashboard's message", apiErr.Body)
	}
	if got != (GovernorStatus{}) {
		t.Errorf("Governor() = %+v, want the zero value on error", got)
	}
}

// TestGovernorEvalIntervalDecodesFixture pins the second endpoint: the eval
// interval is nested under `general_advanced` in a response with many other
// sections, and it is the only field this package reads out of it.
func TestGovernorEvalIntervalDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "governor_config.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").GovernorEvalInterval(context.Background())
	if err != nil {
		t.Fatalf("GovernorEvalInterval() = %v, want nil", err)
	}
	if gotPath != "/api/config/governor" {
		t.Errorf("path = %q, want /api/config/governor", gotPath)
	}
	if want := 5 * time.Minute; got != want {
		t.Errorf("GovernorEvalInterval() = %v, want %v", got, want)
	}
}

// TestGovernorEvalIntervalNonPositive: absent, zero and negative all mean "no
// evaluation scheduled" — the same condition that leaves NextKick empty — and
// none of them may escape as a negative duration a renderer would print as a
// countdown running backwards.
func TestGovernorEvalIntervalNonPositive(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"absent section", `{"thresholds":{"quiet":2}}`},
		{"absent field", `{"general_advanced":{"attribution_trailer":true}}`},
		{"zero", `{"general_advanced":{"eval_interval_s":0}}`},
		{"negative", `{"general_advanced":{"eval_interval_s":-30}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			got, err := newTestClient(t, server, "t").GovernorEvalInterval(context.Background())
			if err != nil {
				t.Fatalf("GovernorEvalInterval() = %v, want nil", err)
			}
			if got != 0 {
				t.Errorf("GovernorEvalInterval() = %v, want 0", got)
			}
		})
	}
}

// TestGovernorEvalIntervalNonOKReturnsAPIError is the config endpoint's error
// path, and it exists to pin the separation of failure domains: a 500 here
// returns an *APIError naming /api/config/governor and NOT /api/status, because
// the two are separate calls and a governor mode that decoded fine must not be
// lost to a config request that did not.
func TestGovernorEvalIntervalNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"config read failed"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").GovernorEvalInterval(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("GovernorEvalInterval() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Path != "/api/config/governor" {
		t.Errorf("Path = %q, want /api/config/governor", apiErr.Path)
	}
	if got != 0 {
		t.Errorf("GovernorEvalInterval() = %v, want 0 on error", got)
	}
}
