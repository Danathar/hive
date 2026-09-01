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

func TestEventsDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "events.json"))
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

	got, err := newTestClient(t, server, "t").Events(context.Background())
	if err != nil {
		t.Fatalf("Events() = %v, want nil", err)
	}
	if gotPath != "/api/audit" {
		t.Errorf("path = %q, want /api/audit", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}

	want := []Event{
		{
			Timestamp: "2026-08-29T16:42:03Z",
			User:      "oidc:00u1opaque",
			UserName:  "Jane Doe",
			Action:    "agent.kick",
			Detail:    "reason=governor",
			Agent:     "scanner",
		},
		{
			Timestamp: "2026-08-29T16:41:20Z",
			User:      "Danathar",
			Action:    "agent.pause",
			Agent:     "reviewer",
		},
		{
			Timestamp: "2026-08-29T16:39:58Z",
			User:      "system",
			Action:    "governor.mode",
			Detail:    "quiet -> busy",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Events() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestEventsEmptyFeed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Events(context.Background())
	if err != nil {
		t.Fatalf("Events() = %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("Events() = %#v, want a non-nil empty slice", got)
	}
}

func TestEventsMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Events(context.Background())
	if err == nil {
		t.Fatal("Events() = nil error on a non-JSON 200, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
	if got != nil {
		t.Errorf("Events() = %+v, want nil on error", got)
	}
}

func TestEventsNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"audit read failed"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Events(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Events() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Path != "/api/audit" {
		t.Errorf("Path = %q, want /api/audit", apiErr.Path)
	}
	if !strings.Contains(apiErr.Body, "audit read failed") {
		t.Errorf("Body = %q, want it to quote the dashboard's message", apiErr.Body)
	}
	if got != nil {
		t.Errorf("Events() = %+v, want nil on error", got)
	}
}
