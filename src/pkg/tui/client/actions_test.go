package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPauseAgentDecodesFixture decodes testdata/actions.json — the response
// schema both pause and resume share — and asserts every field, plus the three
// things about the request the server cares about: method, path and auth.
//
// The method assertion is load-bearing. These are the package's first writes;
// a POST silently issued as a GET would 405 against the real mux, and nothing
// else in this package would have caught the regression.
func TestPauseAgentDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "actions.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod, gotAuth string
	var gotBody []byte
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").PauseAgent(context.Background(), "scanner")
	if err != nil {
		t.Fatalf("PauseAgent() = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/pause/scanner" {
		t.Errorf("path = %q, want /api/pause/scanner", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	// The spec declares no requestBody for this operation, so the client must
	// not invent one — and must not announce a Content-Type for a body it did
	// not send.
	if len(gotBody) != 0 {
		t.Errorf("request body = %q, want empty (spec declares no requestBody)", gotBody)
	}
	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want unset for a bodiless POST", gotContentType)
	}

	want := AgentActionResult{
		OK:      true,
		Status:  "paused",
		Agent:   "scanner",
		Changed: true,
		State:   AgentStatePaused,
	}
	if got != want {
		t.Errorf("PauseAgent() = %+v, want %+v", got, want)
	}
	if !got.Paused() {
		t.Error("Paused() = false, want true for state=paused")
	}
}

// TestResumeAgentSendsExpectedRequest pins resume's own path and method — the
// two methods share a body, so only the prefix distinguishes them and only a
// test keeps them from collapsing onto the same endpoint.
func TestResumeAgentSendsExpectedRequest(t *testing.T) {
	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"status":"resumed","agent":"quality","changed":true,"state":"running"}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ResumeAgent(context.Background(), "quality")
	if err != nil {
		t.Fatalf("ResumeAgent() = %v, want nil", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/resume/quality" {
		t.Errorf("path = %q, want /api/resume/quality", gotPath)
	}

	want := AgentActionResult{OK: true, Status: "resumed", Agent: "quality", Changed: true, State: AgentStateRunning}
	if got != want {
		t.Errorf("ResumeAgent() = %+v, want %+v", got, want)
	}
	// The wire says status "resumed" but state "running": a caller that reads
	// Status as the state gets a value matching neither constant. Paused() is
	// what keeps that mistake out of the panes.
	if got.Paused() {
		t.Error("Paused() = true, want false for state=running")
	}
}

// TestAgentActionNoOp covers the changed=false contract on both endpoints.
//
// This is the case the handler goes out of its way to produce — pausing an
// already-paused agent deliberately does not re-pause it, so as not to clobber
// the original pause reason — and it is a 200, not an error. A caller that
// treats any 200 as "it happened" would report a transition that never
// occurred, which is precisely the stale-UI bug the field exists to fix.
func TestAgentActionNoOp(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		call      func(*Client) (AgentActionResult, error)
		wantState string
	}{
		{
			name:      "pause an already paused agent",
			payload:   `{"ok":true,"status":"paused","agent":"scanner","changed":false,"state":"paused"}`,
			call:      func(c *Client) (AgentActionResult, error) { return c.PauseAgent(context.Background(), "scanner") },
			wantState: AgentStatePaused,
		},
		{
			name:      "resume an agent that is not paused",
			payload:   `{"ok":true,"status":"resumed","agent":"scanner","changed":false,"state":"running"}`,
			call:      func(c *Client) (AgentActionResult, error) { return c.ResumeAgent(context.Background(), "scanner") },
			wantState: AgentStateRunning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.payload)
			}))
			defer server.Close()

			got, err := tc.call(newTestClient(t, server, "tok"))
			if err != nil {
				t.Fatalf("call = %v, want nil (a no-op is a 200, not an error)", err)
			}
			if got.Changed {
				t.Error("Changed = true, want false on a no-op")
			}
			if !got.OK {
				t.Error("OK = false, want true: the handler reports ok on a no-op too")
			}
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q — state is authoritative on the no-op path", got.State, tc.wantState)
			}
		})
	}
}

// TestAgentActionEscapesAgentName pins that the name is escaped into its path
// segment. A name carrying a separator must not be able to retarget the
// request at another route.
func TestAgentActionEscapesAgentName(t *testing.T) {
	var gotRawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not URL.Path: the latter is already decoded, so it
		// cannot show whether the client escaped anything.
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"status":"paused","agent":"a/b","changed":true,"state":"paused"}`)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server, "tok").PauseAgent(context.Background(), "a/b"); err != nil {
		t.Fatalf("PauseAgent() = %v, want nil", err)
	}
	if strings.Contains(gotRawPath, "a/b") {
		t.Errorf("escaped path = %q, want the separator escaped, not passed through", gotRawPath)
	}
	if gotRawPath != "/api/pause/a%2Fb" {
		t.Errorf("escaped path = %q, want /api/pause/a%%2Fb", gotRawPath)
	}
}

// TestAgentActionRequiresAgentName checks the empty name fails locally.
//
// Addressing "/api/pause/" matches no route, so without this the caller's
// mistake comes back as a 404 describing the routing table. It must also not
// reach the network at all.
func TestAgentActionRequiresAgentName(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	c := newTestClient(t, server, "tok")

	for _, tc := range []struct {
		name string
		call func() (AgentActionResult, error)
	}{
		{"pause", func() (AgentActionResult, error) { return c.PauseAgent(context.Background(), "") }},
		{"resume", func() (AgentActionResult, error) { return c.ResumeAgent(context.Background(), "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil {
				t.Fatal("error = nil, want a rejection of the empty agent name")
			}
			if !strings.Contains(err.Error(), "agent name is required") {
				t.Errorf("error = %q, does not name the problem", err)
			}
		})
	}
	if called {
		t.Error("an empty agent name reached the server; it must fail before the request")
	}
}

// TestAgentActionNonOKReturnsAPIError covers the failure statuses these
// endpoints actually document (400 unknown agent, 403 owner gate) plus the 500
// the task's acceptance criteria call for, and pins that the error names the
// method it used.
func TestAgentActionNonOKReturnsAPIError(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"unknown agent", http.StatusBadRequest, `{"ok":false,"error":"unknown agent \"nope\""}`},
		{"owner gate", http.StatusForbidden, `{"ok":false,"error":"owner access required"}`},
		{"server error", http.StatusInternalServerError, `{"ok":false,"error":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			got, err := newTestClient(t, server, "tok").PauseAgent(context.Background(), "nope")
			if err == nil {
				t.Fatalf("PauseAgent() error = nil, want *APIError for %d", tc.code)
			}
			if got != (AgentActionResult{}) {
				t.Errorf("result = %+v, want the zero value on error", got)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.code)
			}
			// The regression this guards: APIError used to hardcode "GET", so
			// a failed write reported a request that was never made.
			if apiErr.Method != http.MethodPost {
				t.Errorf("Method = %q, want POST", apiErr.Method)
			}
			if !strings.HasPrefix(apiErr.Error(), "POST /api/pause/nope:") {
				t.Errorf("Error() = %q, want it to name the POST and the path", apiErr.Error())
			}
			if !strings.Contains(apiErr.Error(), http.StatusText(tc.code)) {
				t.Errorf("Error() = %q, does not name the status", apiErr.Error())
			}
		})
	}
}

// TestIsForbidden pins the one failure a pane must render differently: a 403 is
// a working request from a non-owner, and retrying it — the right response to
// most other errors — will never help.
func TestIsForbidden(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 from the owner gate", &APIError{StatusCode: http.StatusForbidden, Method: "POST", Path: "/api/pause/x"}, true},
		{"401 bad token", &APIError{StatusCode: http.StatusUnauthorized, Method: "POST", Path: "/api/pause/x"}, false},
		{"500 server error", &APIError{StatusCode: http.StatusInternalServerError, Method: "POST", Path: "/api/pause/x"}, false},
		// errors.As must see through a wrap: the panes will hand this errors
		// that travelled up through a message type, not the bare APIError.
		{"wrapped 403 stays recognisable", fmt.Errorf("pausing scanner: %w", &APIError{StatusCode: http.StatusForbidden, Method: "POST", Path: "/api/pause/x"}), true},
		{"an ordinary error is not a 403", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsForbidden(tc.err); got != tc.want {
				t.Errorf("IsForbidden(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAgentActionDecodeErrorIsNotAPIError: a 200 carrying something that is not
// this schema is a decode failure, not an API error. The distinction matters —
// a pane retries a transport blip and reports a malformed payload.
func TestAgentActionDecodeErrorIsNotAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `["not", "an", "object"]`)
	}))
	defer server.Close()

	_, err := newTestClient(t, server, "tok").PauseAgent(context.Background(), "scanner")
	if err == nil {
		t.Fatal("PauseAgent() = nil, want a decode error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("decode failure surfaced as *APIError (%v); the response WAS a 200", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, does not name the decode failure", err)
	}
}

// TestKickAgentDecodesFixtureAndSendsPrompt pins the response schema and the
// prompt-bearing request form published for POST /api/kick/{agent}.
func TestKickAgentDecodesFixtureAndSendsPrompt(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "kick.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod, gotAuth, gotContentType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").KickAgent(context.Background(), "scanner", "review issue 5217")
	if err != nil {
		t.Fatalf("KickAgent() = %v, want nil", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/kick/scanner" {
		t.Errorf("path = %q, want /api/kick/scanner", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if want := `{"prompt":"review issue 5217"}`; string(gotBody) != want {
		t.Errorf("request body = %q, want %q", gotBody, want)
	}
	if want := (KickResult{Status: "kicked", Agent: "scanner"}); got != want {
		t.Errorf("KickAgent() = %+v, want %+v", got, want)
	}
}

// TestKickAgentWithoutPromptSendsNoBody protects the server's automatic-kick
// path. Sending an empty prompt object would be observably different from the
// operation's optional request body and would mask regressions in postJSON.
func TestKickAgentWithoutPromptSendsNoBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"kicked","agent":"quality"}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").KickAgent(context.Background(), "quality", "")
	if err != nil {
		t.Fatalf("KickAgent() = %v, want nil", err)
	}
	if len(gotBody) != 0 {
		t.Errorf("request body = %q, want empty for an automatic kick", gotBody)
	}
	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want unset for a bodiless POST", gotContentType)
	}
	if want := (KickResult{Status: "kicked", Agent: "quality"}); got != want {
		t.Errorf("KickAgent() = %+v, want %+v", got, want)
	}
}

// TestKickAgentEscapesAndRequiresAgentName keeps the agent argument inside its
// one path segment and rejects an empty path parameter before any request.
func TestKickAgentEscapesAndRequiresAgentName(t *testing.T) {
	var gotRawPath string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"kicked","agent":"a/b"}`)
	}))
	defer server.Close()
	c := newTestClient(t, server, "tok")

	if _, err := c.KickAgent(context.Background(), "a/b", "go"); err != nil {
		t.Fatalf("KickAgent() = %v, want nil", err)
	}
	if gotRawPath != "/api/kick/a%2Fb" {
		t.Errorf("escaped path = %q, want /api/kick/a%%2Fb", gotRawPath)
	}
	if _, err := c.KickAgent(context.Background(), "", "go"); err == nil {
		t.Fatal("KickAgent() error = nil, want a rejection of the empty agent name")
	} else if !strings.Contains(err.Error(), "agent name is required") {
		t.Errorf("error = %q, does not name the problem", err)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1; empty agent must fail locally", calls)
	}
}

// TestKickAgentServerErrorReturnsAPIError covers the task's required error
// path and ensures failed kicks retain their POST method and escaped path.
func TestKickAgentServerErrorReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").KickAgent(context.Background(), "scanner", "go")
	if err == nil {
		t.Fatal("KickAgent() error = nil, want *APIError")
	}
	if got != (KickResult{}) {
		t.Errorf("result = %+v, want the zero value on error", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if apiErr.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", apiErr.Method)
	}
	if apiErr.Path != "/api/kick/scanner" {
		t.Errorf("Path = %q, want /api/kick/scanner", apiErr.Path)
	}
}
