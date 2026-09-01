package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTokensDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "tokens.json"))
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

	got, err := newTestClient(t, server, "t").Tokens(context.Background())
	if err != nil {
		t.Fatalf("Tokens() = %v, want nil", err)
	}
	if gotPath != "/api/tokens" {
		t.Errorf("path = %q, want /api/tokens", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}

	want := TokenUsage{
		TotalTokens:      1_883_700,
		TotalInput:       1_296_700,
		TotalOutput:      100_500,
		TotalCacheRead:   475_000,
		TotalCacheCreate: 11_500,
		TotalMessages:    47,
		ByAgent: map[string]int64{
			"scanner":  1_748_100,
			"reviewer": 135_600,
		},
		ByModel: map[string]int64{
			"claude-opus-4-5":   1_748_100,
			"claude-sonnet-4-5": 135_600,
		},
		ByAgentDetail: map[string]*TokenBucket{
			"scanner":  {Input: 1_200_000, Output: 88_100, CacheRead: 450_000, CacheCreate: 10_000, Messages: 42, Sessions: 1},
			"reviewer": {Input: 96_700, Output: 12_400, CacheRead: 25_000, CacheCreate: 1_500, Messages: 5, Sessions: 1},
		},
		ByModelDetail: map[string]*TokenBucket{
			"claude-opus-4-5":   {Input: 1_200_000, Output: 88_100, CacheRead: 450_000, CacheCreate: 10_000, Messages: 42, Sessions: 1},
			"claude-sonnet-4-5": {Input: 96_700, Output: 12_400, CacheRead: 25_000, CacheCreate: 1_500, Messages: 5, Sessions: 1},
		},
		Sessions: []TokenSession{
			{
				SessionID: "session-scanner-01", Agent: "scanner", Model: "claude-opus-4-5",
				InputTokens: 1_200_000, OutputTokens: 88_100, CacheRead: 450_000,
				CacheCreate: 10_000, TotalTokens: 1_748_100, Messages: 42,
				FirstActive: 1_788_015_600_000, LastActive: 1_788_016_500_000, Backend: "claude",
				Usage: []TokenUsageEvent{
					{TimestampMs: 1_788_015_600_000, Model: "claude-opus-4-5", Coalesced: 2, Input: 700_000, Output: 50_000, CacheRead: 250_000, CacheCreate: 6_000},
					{TimestampMs: 1_788_016_500_000, Model: "claude-opus-4-5", Input: 500_000, Output: 38_100, CacheRead: 200_000, CacheCreate: 4_000},
				},
				UsageCoalesced: 1,
			},
			{
				SessionID: "session-reviewer-01", Agent: "reviewer", Model: "claude-sonnet-4-5",
				InputTokens: 96_700, OutputTokens: 12_400, CacheRead: 25_000,
				CacheCreate: 1_500, TotalTokens: 135_600, Messages: 5,
				FirstActive: 1_788_016_200_000, LastActive: 1_788_016_800_000, Backend: "claude",
				Usage: []TokenUsageEvent{
					{TimestampMs: 1_788_016_800_000, Model: "claude-sonnet-4-5", Input: 96_700, Output: 12_400, CacheRead: 25_000, CacheCreate: 1_500},
				},
			},
		},
		SessionCount: 2,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokens() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestTokensNoCollector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"no_collector"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Tokens(context.Background())
	if err != nil {
		t.Fatalf("Tokens() = %v, want nil", err)
	}
	if got.Status != "no_collector" {
		t.Errorf("Status = %q, want no_collector", got.Status)
	}
	if got.TotalTokens != 0 || len(got.Sessions) != 0 {
		t.Errorf("Tokens() = %+v, want no-collector status and zero usage", got)
	}
}

func TestTokensEmptySummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_tokens":0,"sessions":[]}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Tokens(context.Background())
	if err != nil {
		t.Fatalf("Tokens() = %v, want nil", err)
	}
	if got.Status != "" || got.TotalTokens != 0 || got.Sessions == nil || len(got.Sessions) != 0 {
		t.Errorf("Tokens() = %+v, want an empty non-nil sessions slice and zero totals", got)
	}
}

func TestTokensMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Tokens(context.Background())
	if err == nil {
		t.Fatal("Tokens() = nil error on a non-JSON 200, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
	if !reflect.DeepEqual(got, TokenUsage{}) {
		t.Errorf("Tokens() = %+v, want the zero value on error", got)
	}
}

func TestTokensNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"token collection failed"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Tokens(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Tokens() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Path != "/api/tokens" {
		t.Errorf("Path = %q, want /api/tokens", apiErr.Path)
	}
	if !strings.Contains(apiErr.Body, "token collection failed") {
		t.Errorf("Body = %q, want it to quote the dashboard's message", apiErr.Body)
	}
	if !reflect.DeepEqual(got, TokenUsage{}) {
		t.Errorf("Tokens() = %+v, want the zero value on error", got)
	}
}
