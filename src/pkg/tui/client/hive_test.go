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
)

func TestHiveIDDecodesFixtureAndSendsExpectedRequest(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "hive.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "dashboard-secret").HiveID(context.Background())
	if err != nil {
		t.Fatalf("HiveID() = %v, want nil", err)
	}
	if got != "production-east" {
		t.Errorf("HiveID() = %q, want production-east", got)
	}
	if gotPath != "/api/hive-id" {
		t.Errorf("path = %q, want /api/hive-id", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotAuth != "Bearer dashboard-secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer dashboard-secret")
	}
}

func TestHiveIDPreservesEmptyIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":""}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").HiveID(context.Background())
	if err != nil {
		t.Fatalf("HiveID() = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("HiveID() = %q, want empty", got)
	}
}

func TestHiveIDMalformedBodyReturnsDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").HiveID(context.Background())
	if err == nil {
		t.Fatal("HiveID() = nil error on a non-JSON 200, want a decode error")
	}
	if got != "" {
		t.Errorf("HiveID() = %q, want empty on error", got)
	}
	if !strings.Contains(err.Error(), "decode /api/hive-id") {
		t.Fatalf("error = %v, want it to name the endpoint decode failure", err)
	}
}

func TestHiveIDNonOKReturnsBoundedAPIError(t *testing.T) {
	body := strings.Repeat("x", maxErrorBodyBytes*2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").HiveID(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("HiveID() error = %v (%T), want *APIError", err, err)
	}
	if got != "" {
		t.Errorf("HiveID() = %q, want empty on error", got)
	}
	if apiErr.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", apiErr.Method)
	}
	if apiErr.Path != "/api/hive-id" {
		t.Errorf("Path = %q, want /api/hive-id", apiErr.Path)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadGateway)
	}
	if apiErr.Body != body[:maxErrorBodyBytes] {
		t.Errorf("Body length = %d, want bounded prefix of %d bytes", len(apiErr.Body), maxErrorBodyBytes)
	}
}
