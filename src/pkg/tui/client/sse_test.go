package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamEventsFramingAuthAndMidEventClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sseEventsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, sseEventsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer stream-secret" {
			t.Errorf("Authorization = %q, want Bearer stream-secret", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest response writer does not support flushing")
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Split fields across writes and flushes so the reader cannot assume
		// one network read contains one line or one event.
		write := func(fragment string) {
			t.Helper()
			if _, err := io.WriteString(w, fragment); err != nil {
				t.Errorf("write stream fragment: %v", err)
				return
			}
			flusher.Flush()
		}
		write(":keep")
		write("alive\r\n")
		write("data: {\"timestamp\":\"one\",\"hiveId\":\"demo\"}\r\n\r\n")
		write(": another keepalive\n\n")
		write("event: agent-status\n")
		write("data: {\"timestamp\":\"two\",\n")
		write("data: \"govMode\":\"busy\"}\n\n")

		// Returning here closes the connection in the middle of this frame.
		// It must produce an error without delivering a third event.
		write("event: agent-status\ndata: {\"timestamp\":\"cut")
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL)
	t.Setenv(TokenEnv, "stream-secret")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, terminal := collectStream(t, ctx, New())
	if len(events) != 2 {
		t.Fatalf("received %d events, want 2 complete events", len(events))
	}
	if events[0].Type != SSEEventTypeMessage {
		t.Errorf("first event type = %q, want %q", events[0].Type, SSEEventTypeMessage)
	}
	if events[1].Type != SSEEventTypeAgentStatus {
		t.Errorf("second event type = %q, want %q", events[1].Type, SSEEventTypeAgentStatus)
	}
	if !strings.Contains(string(events[1].Data), "\n") {
		t.Errorf("multi-line data was not joined with a newline: %q", events[1].Data)
	}

	var full struct {
		Timestamp string `json:"timestamp"`
		HiveID    string `json:"hiveId"`
	}
	if err := events[0].Decode(&full); err != nil {
		t.Fatalf("decode full status event: %v", err)
	}
	if full.Timestamp != "one" || full.HiveID != "demo" {
		t.Errorf("full status = %+v, want timestamp one and hive demo", full)
	}

	var agents struct {
		Timestamp string `json:"timestamp"`
		GovMode   string `json:"govMode"`
	}
	if err := events[1].Decode(&agents); err != nil {
		t.Fatalf("decode agent-status event: %v", err)
	}
	if agents.Timestamp != "two" || agents.GovMode != "busy" {
		t.Errorf("agent status = %+v, want timestamp two and govMode busy", agents)
	}
	if !errors.Is(terminal, io.ErrUnexpectedEOF) {
		t.Fatalf("terminal error = %v, want io.ErrUnexpectedEOF", terminal)
	}
}

func TestStreamEventsCleanClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"timestamp\":\"complete\"}\n\n")
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL)
	t.Setenv(TokenEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, terminal := collectStream(t, ctx, New())
	if len(events) != 1 {
		t.Fatalf("received %d events, want 1", len(events))
	}
	if terminal != nil {
		t.Fatalf("clean connection close returned error: %v", terminal)
	}
}

func TestStreamEventsRejectsInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: not-json\n\n")
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, terminal := collectStream(t, ctx, New())
	if len(events) != 0 {
		t.Fatalf("received %d events from invalid JSON, want none", len(events))
	}
	if terminal == nil || !strings.Contains(terminal.Error(), "invalid JSON") {
		t.Fatalf("terminal error = %v, want invalid JSON error", terminal)
	}
}

func TestStreamEventsCancellationClosesChannelsWithoutError(t *testing.T) {
	connected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(connected)
		<-r.Context().Done()
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	eventsCh, errsCh := New().StreamEvents(ctx)
	<-connected
	cancel()

	for eventsCh != nil || errsCh != nil {
		select {
		case _, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
			}
		case err, ok := <-errsCh:
			if !ok {
				errsCh = nil
			} else {
				t.Fatalf("cancellation returned terminal error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stream channels did not close after cancellation")
		}
	}
}

func collectStream(t *testing.T, ctx context.Context, c *Client) ([]SSEEvent, error) {
	t.Helper()
	eventsCh, errsCh := c.StreamEvents(ctx)
	var events []SSEEvent
	var terminal error

	for eventsCh != nil || errsCh != nil {
		select {
		case event, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
			} else {
				events = append(events, event)
			}
		case err, ok := <-errsCh:
			if !ok {
				errsCh = nil
			} else {
				terminal = err
			}
		case <-ctx.Done():
			t.Fatalf("timed out collecting stream: %v", ctx.Err())
		}
	}
	return events, terminal
}
