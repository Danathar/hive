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

// TestSSEEventDecode covers both arms of Decode: a payload that matches the
// caller's type, and one that does not.
//
// Decode is the seam T13b uses to turn a raw event into a pane's own snapshot
// type, so the error arm matters — a status event whose shape has drifted must
// name the event type it failed on, not just "unmarshal error".
func TestSSEEventDecode(t *testing.T) {
	t.Run("decodes into the caller's type", func(t *testing.T) {
		e := SSEEvent{Type: "status", Data: []byte(`{"hive":"demo","agents":3}`)}
		var got struct {
			Hive   string `json:"hive"`
			Agents int    `json:"agents"`
		}
		if err := e.Decode(&got); err != nil {
			t.Fatalf("Decode() = %v, want nil", err)
		}
		if got.Hive != "demo" || got.Agents != 3 {
			t.Errorf("decoded %+v, want {demo 3}", got)
		}
	})

	t.Run("names the event type on a mismatch", func(t *testing.T) {
		e := SSEEvent{Type: "status", Data: []byte(`{"agents":"three"}`)}
		var got struct {
			Agents int `json:"agents"`
		}
		err := e.Decode(&got)
		if err == nil {
			t.Fatal("Decode() = nil, want a type error")
		}
		if !strings.Contains(err.Error(), "status") {
			t.Errorf("error = %q, does not name the event type it failed on", err)
		}
	})
}

// TestStreamEventsNonOKReturnsAPIError: a dashboard that answers the stream
// request with a status rather than a stream must surface as the same typed
// error the polling calls produce, so a caller can tell 401 (bad token) from
// 502 (proxy up, API down) exactly as it can elsewhere.
func TestStreamEventsNonOKReturnsAPIError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":"nope"}`)
			}))
			defer server.Close()

			t.Setenv(BaseURLEnv, server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			events, terminal := collectStream(t, ctx, New())
			if len(events) != 0 {
				t.Errorf("received %d events from a %d response, want none", len(events), code)
			}
			var apiErr *APIError
			if !errors.As(terminal, &apiErr) {
				t.Fatalf("terminal error = %v (%T), want *APIError", terminal, terminal)
			}
			if apiErr.StatusCode != code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, code)
			}
			if apiErr.Path != sseEventsPath {
				t.Errorf("Path = %q, want %q", apiErr.Path, sseEventsPath)
			}
		})
	}
}

// TestStreamEventsOmitsAuthorizationWithoutToken mirrors the polling client's
// equivalent: an unset HIVE_DASHBOARD_TOKEN must send no header at all rather
// than an empty "Bearer ".
func TestStreamEventsOmitsAuthorizationWithoutToken(t *testing.T) {
	var hadAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL)
	t.Setenv(TokenEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	collectStream(t, ctx, New())
	if hadAuth {
		t.Error("Authorization header was sent without a token configured")
	}
}

// TestStreamEventsMalformedBaseURLFailsBeforeConnecting: the request cannot be
// built at all, which must surface as a plain error rather than a panic or a
// silent empty stream. New() cannot fail by contract, so this is where a bad
// HIVE_DASHBOARD_URL is finally reported.
func TestStreamEventsMalformedBaseURLFailsBeforeConnecting(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://\x7f-invalid")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, terminal := collectStream(t, ctx, New())
	if len(events) != 0 {
		t.Errorf("received %d events from an unbuildable request, want none", len(events))
	}
	if terminal == nil {
		t.Fatal("terminal error = nil, want the request-build failure")
	}
	if !strings.Contains(terminal.Error(), sseEventsPath) {
		t.Errorf("error = %q, does not name the path it failed to build", terminal)
	}
}

// readEventStream is exercised directly here rather than through a server: the
// remaining uncovered branches are framing edge cases, and driving them over
// HTTP would add a server per case without testing anything more.
func TestReadEventStreamFramingEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []SSEEvent
	}{
		{
			name:  "comment lines are ignored",
			input: ": keep-alive\ndata: {\"a\":1}\n\n",
			want:  []SSEEvent{{Type: SSEEventTypeMessage, Data: []byte(`{"a":1}`)}},
		},
		{
			name: "an empty event: value resets to the default type",
			// The second event must NOT inherit "status" from the first.
			input: "event: status\ndata: {\"a\":1}\n\nevent:\ndata: {\"b\":2}\n\n",
			want: []SSEEvent{
				{Type: "status", Data: []byte(`{"a":1}`)},
				{Type: SSEEventTypeMessage, Data: []byte(`{"b":2}`)},
			},
		},
		{
			name:  "a field line with no colon is a field with an empty value",
			input: "data\ndata: {\"a\":1}\n\n",
			want:  []SSEEvent{{Type: SSEEventTypeMessage, Data: []byte("\n" + `{"a":1}`)}},
		},
		{
			name:  "an unknown field is ignored",
			input: "id: 7\nretry: 5000\ndata: {\"a\":1}\n\n",
			want:  []SSEEvent{{Type: SSEEventTypeMessage, Data: []byte(`{"a":1}`)}},
		},
		{
			name:  "a blank line with no data dispatches nothing",
			input: "\n\nevent: status\n\ndata: {\"a\":1}\n\n",
			want:  []SSEEvent{{Type: SSEEventTypeMessage, Data: []byte(`{"a":1}`)}},
		},
		{
			name:  "CRLF line endings are accepted",
			input: "event: status\r\ndata: {\"a\":1}\r\n\r\n",
			want:  []SSEEvent{{Type: "status", Data: []byte(`{"a":1}`)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []SSEEvent
			err := readEventStream(context.Background(), strings.NewReader(tc.input), func(e SSEEvent) error {
				got = append(got, e)
				return nil
			})
			if err != nil {
				t.Fatalf("readEventStream() = %v, want nil", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events (%+v), want %d", len(got), got, len(tc.want))
			}
			for i := range tc.want {
				if got[i].Type != tc.want[i].Type {
					t.Errorf("event %d type = %q, want %q", i, got[i].Type, tc.want[i].Type)
				}
				if string(got[i].Data) != string(tc.want[i].Data) {
					t.Errorf("event %d data = %q, want %q", i, got[i].Data, tc.want[i].Data)
				}
			}
		})
	}
}

// A line longer than the scanner's 8MB cap is a read failure, not a silent
// truncation: half an event decoded as if whole would be worse than no event.
func TestReadEventStreamLineTooLong(t *testing.T) {
	huge := "data: " + strings.Repeat("x", 9<<20) + "\n\n"
	err := readEventStream(context.Background(), strings.NewReader(huge), func(SSEEvent) error {
		t.Error("an over-long line produced an event; it must fail instead")
		return nil
	})
	if err == nil {
		t.Fatal("readEventStream() = nil, want a read error")
	}
	if !strings.Contains(err.Error(), sseEventsPath) {
		t.Errorf("error = %q, does not name the stream it failed to read", err)
	}
}

// A cancelled context stops the loop even while lines are still available, and
// reports ctx.Err() rather than a stream error — a cancelled stream is not a
// broken one.
func TestReadEventStreamStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := readEventStream(ctx, strings.NewReader("data: {\"a\":1}\n\n"), func(SSEEvent) error {
		t.Error("emitted an event under a cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readEventStream() = %v, want context.Canceled", err)
	}
}

// An emit that fails aborts the stream and surfaces its error unchanged. This
// is the path StreamEvents uses to stop when its consumer's context is done.
func TestReadEventStreamPropagatesEmitError(t *testing.T) {
	sentinel := errors.New("consumer went away")
	err := readEventStream(context.Background(), strings.NewReader("data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"),
		func(SSEEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("readEventStream() = %v, want the emit error", err)
	}
}
