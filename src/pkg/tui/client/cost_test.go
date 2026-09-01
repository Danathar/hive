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

func TestCostsDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "cost.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "cost-token").Costs(context.Background())
	if err != nil {
		t.Fatalf("Costs() = %v, want nil", err)
	}
	if gotPath != "/api/cost" {
		t.Errorf("path = %q, want /api/cost", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotAuthorization != "Bearer cost-token" {
		t.Errorf("Authorization = %q, want Bearer cost-token", gotAuthorization)
	}

	want := CostSummary{
		TotalUSD: 7.375,
		ByAgent: []CostAgentEntry{
			{Name: "scanner", USD: 7.375, Source: "estimated", Input: 1_200_000, Output: 88_100, CacheRead: 450_000, CacheCreate: 10_000},
			{Name: "quality", USD: 0, Source: "estimated", Input: 0, Output: 0, CacheRead: 0, CacheCreate: 0},
			{Name: "reviewer", USD: 0, Source: "unpriced", Input: 96_700, Output: 12_400, CacheRead: 25_000, CacheCreate: 1_500},
		},
		UnpricedModels: []string{"private-model-v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Costs() =\n%+v\nwant\n%+v", got, want)
	}

	if !got.ByAgent[1].Known() {
		t.Error("estimated $0.00 entry Known() = false, want true")
	}
	if got.ByAgent[2].Known() {
		t.Error("unpriced $0.00 entry Known() = true, want false")
	}
	if got.AllPriced() {
		t.Error("mixed priced/unpriced summary AllPriced() = true, want false")
	}
}

func TestCostsEmptySummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"estimated":{"total_usd":0,"by_agent":[],"unpriced_models":[]}}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Costs(context.Background())
	if err != nil {
		t.Fatalf("Costs() = %v, want nil", err)
	}
	if got.TotalUSD != 0 || got.ByAgent == nil || len(got.ByAgent) != 0 || got.UnpricedModels == nil || len(got.UnpricedModels) != 0 {
		t.Errorf("Costs() = %+v, want zero total and empty non-nil arrays", got)
	}
	if !got.AllPriced() {
		t.Error("empty no-usage summary AllPriced() = false, want true")
	}
}

func TestCostsMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Costs(context.Background())
	if err == nil {
		t.Fatal("Costs() = nil error on a non-JSON 200, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
	if !reflect.DeepEqual(got, CostSummary{}) {
		t.Errorf("Costs() = %+v, want the zero value on error", got)
	}
}

func TestCostsNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"cost estimate failed"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Costs(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Costs() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", apiErr.Method)
	}
	if apiErr.Path != "/api/cost" {
		t.Errorf("Path = %q, want /api/cost", apiErr.Path)
	}
	if !strings.Contains(apiErr.Body, "cost estimate failed") {
		t.Errorf("Body = %q, want it to quote the dashboard's message", apiErr.Body)
	}
	if !reflect.DeepEqual(got, CostSummary{}) {
		t.Errorf("Costs() = %+v, want the zero value on error", got)
	}
}
