package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const sseEventsPath = "/api/events"

// SSEEventType is the value of an SSE event field. A frame without one is a
// standard message event.
type SSEEventType string

const (
	SSEEventTypeMessage     SSEEventType = "message"
	SSEEventTypeAgentStatus SSEEventType = "agent-status"
)

// SSEEvent is one completely framed dashboard SSE message.
//
// Data is validated JSON but stays raw so T13b can decode a full status event
// and an agent-status event into their distinct pane delivery types without
// this transport layer depending on every pane's model.
type SSEEvent struct {
	Type SSEEventType
	Data json.RawMessage
}

// Decode unmarshals an event payload into the caller's typed snapshot.
func (e SSEEvent) Decode(v any) error {
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("decode %s event: %w", e.Type, err)
	}
	return nil
}

// StreamEvents connects to the dashboard's SSE endpoint and returns channels
// for complete events and the first terminal stream error.
//
// Both channels close when the server closes cleanly or ctx is cancelled. An
// EOF in the middle of an event is reported as io.ErrUnexpectedEOF; no partial
// event is delivered. Reconnection belongs to the caller (T13b).
//
// This adapts rather than reuses hivectl.Client.StreamSSE. That helper has the
// correct request/auth pattern, but its public contract writes JSON-wrapped raw
// lines to an io.Writer. The TUI needs parsed event/data framing on a channel,
// so sharing it would require parsing an encoded intermediate representation.
func (c *Client) StreamEvents(ctx context.Context) (<-chan SSEEvent, <-chan error) {
	events := make(chan SSEEvent)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		if err := c.streamEvents(ctx, events); err != nil && ctx.Err() == nil {
			errs <- err
		}
	}()

	return events, errs
}

func (c *Client) streamEvents(ctx context.Context, events chan<- SSEEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+sseEventsPath, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", sseEventsPath, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// Client.Timeout includes reading the entire response body, which is right
	// for polling but would kill every healthy long-lived stream after five
	// seconds. Copy the client so the stream has only its context lifetime;
	// the shared transport and its connection policy remain unchanged.
	streamHTTP := *c.http
	streamHTTP.Timeout = 0
	resp, err := streamHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("GET %s: %w", sseEventsPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &APIError{
			StatusCode: resp.StatusCode,
			Path:       sseEventsPath,
			Body:       strings.TrimSpace(string(body)),
		}
	}

	return readEventStream(ctx, resp.Body, func(event SSEEvent) error {
		select {
		case events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func readEventStream(ctx context.Context, src io.Reader, emit func(SSEEvent) error) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)

	eventType := SSEEventTypeMessage
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			eventType = SSEEventTypeMessage
			return nil
		}

		data := []byte(strings.Join(dataLines, "\n"))
		if !json.Valid(data) {
			return fmt.Errorf("decode %s event: invalid JSON", eventType)
		}
		event := SSEEvent{
			Type: eventType,
			Data: append(json.RawMessage(nil), data...),
		}
		eventType = SSEEventTypeMessage
		dataLines = dataLines[:0]
		return emit(event)
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			if value == "" {
				eventType = SSEEventTypeMessage
			} else {
				eventType = SSEEventType(value)
			}
		case "data":
			dataLines = append(dataLines, value)
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read %s stream: %w", sseEventsPath, err)
	}
	if len(dataLines) != 0 {
		return fmt.Errorf("read %s stream: %w", sseEventsPath, io.ErrUnexpectedEOF)
	}
	return nil
}
