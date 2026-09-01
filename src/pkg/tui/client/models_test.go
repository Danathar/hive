package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelsDecodesFixture decodes testdata/models.json and asserts every field
// of the envelope, including the two the published spec omits entirely
// (entitled, entitledSource — see #5077).
//
// The models assertion is the load-bearing one: the spec declares the array's
// items as objects, the endpoint sends bare strings, and a client that believed
// the spec would decode nothing at all here.
func TestModelsDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "models.json"))
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

	got, err := newTestClient(t, server, "tok").Models(context.Background(), "litellm")
	if err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/inference/models/litellm" {
		t.Errorf("path = %q, want /api/inference/models/litellm", gotPath)
	}

	if got.Backend != "litellm" {
		t.Errorf("Backend = %q, want litellm", got.Backend)
	}
	wantModels := []ModelOption{"openai/gpt-5", "anthropic/claude-opus-5", "gemini_2.5_pro"}
	if len(got.Models) != len(wantModels) {
		t.Fatalf("Models = %v, want %v", got.Models, wantModels)
	}
	for i := range wantModels {
		if got.Models[i] != wantModels[i] {
			t.Errorf("Models[%d] = %q, want %q", i, got.Models[i], wantModels[i])
		}
	}
	if got.Fallback {
		t.Error("Fallback = true, want false")
	}
	if got.Partial {
		t.Error("Partial = true, want false")
	}
	if !got.Entitled {
		t.Error("Entitled = false, want true — the field is absent from the spec but the handler sends it")
	}
	if got.EntitledSource != "key-info" {
		t.Errorf("EntitledSource = %q, want key-info", got.EntitledSource)
	}
	if !got.Authoritative() {
		t.Error("Authoritative() = false, want true for a complete non-fallback discovery")
	}
}

// TestModelOptionDecodesFromBareString is the direct statement of the drift:
// the wire carries strings where the spec promises objects. If ModelOption ever
// grows into a struct to "match the spec", this is what fails.
func TestModelOptionDecodesFromBareString(t *testing.T) {
	var got []ModelOption
	if err := json.Unmarshal([]byte(`["a","b/c"]`), &got); err != nil {
		t.Fatalf("unmarshal = %v; ModelOption must decode from a bare JSON string", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b/c" {
		t.Fatalf("got %v, want [a b/c]", got)
	}
	// Round-trips, so a caller that re-marshals a decoded list sends back what
	// it received rather than a shape the server does not accept.
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal = %v", err)
	}
	if string(out) != `["a","b/c"]` {
		t.Errorf("marshal = %s, want [\"a\",\"b/c\"]", out)
	}
}

// TestModelsAuthoritative covers the qualifier that decides whether a model's
// ABSENCE from the list means anything. Getting this wrong is #4438: auto-heal
// reading a partial sample as a census switches an agent off a model that only
// the unreachable endpoint served.
func TestModelsAuthoritative(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		fallback bool
		partial  bool
		want     bool
	}{
		{"complete discovery", `{"backend":"vllm","models":["a"],"fallback":false,"partial":false}`, false, false, true},
		{"static fallback is unverified", `{"backend":"vllm","models":["a"],"fallback":true,"partial":false}`, true, false, false},
		{"partial sample is a floor", `{"backend":"vllm","models":["a"],"fallback":false,"partial":true}`, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.payload)
			}))
			defer server.Close()

			got, err := newTestClient(t, server, "tok").Models(context.Background(), "vllm")
			if err != nil {
				t.Fatalf("Models() = %v, want nil", err)
			}
			if got.Fallback != tc.fallback {
				t.Errorf("Fallback = %v, want %v", got.Fallback, tc.fallback)
			}
			if got.Partial != tc.partial {
				t.Errorf("Partial = %v, want %v", got.Partial, tc.partial)
			}
			if got.Authoritative() != tc.want {
				t.Errorf("Authoritative() = %v, want %v", got.Authoritative(), tc.want)
			}
		})
	}
}

// TestModelsEmptyAndAbsentFields: a backend that offers nothing decodes to an
// empty list, not an error, and the two optional fields are simply absent on
// the responses that do not carry them (which is most of them — the handler
// sets entitled only for a litellm backend whose entitled set it has learned).
func TestModelsEmptyAndAbsentFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"backend":"vllm","models":[],"fallback":false,"partial":false}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").Models(context.Background(), "vllm")
	if err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	if len(got.Models) != 0 {
		t.Errorf("Models = %v, want empty", got.Models)
	}
	if got.Entitled || got.EntitledSource != "" {
		t.Errorf("Entitled=%v EntitledSource=%q, want the zero values when absent", got.Entitled, got.EntitledSource)
	}
}

// TestModelsEscapesBackend pins that the backend is escaped into its path
// segment, matching the encodeURIComponent the dashboard already applies here.
func TestModelsEscapesBackend(t *testing.T) {
	var gotRawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not URL.Path: the latter is already decoded, so it
		// cannot show whether the client escaped anything.
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"backend":"a/b","models":[],"fallback":false,"partial":false}`)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server, "tok").Models(context.Background(), "a/b"); err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	if gotRawPath != "/api/inference/models/a%2Fb" {
		t.Errorf("escaped path = %q, want /api/inference/models/a%%2Fb", gotRawPath)
	}
}

// TestModelsRequiresBackend checks the empty backend fails locally, without a
// request: "/api/inference/models/" matches no route, so the server's answer
// would describe the routing table rather than the caller's mistake.
func TestModelsRequiresBackend(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	_, err := newTestClient(t, server, "tok").Models(context.Background(), "")
	if err == nil {
		t.Fatal("error = nil, want a rejection of the empty backend")
	}
	if !strings.Contains(err.Error(), "backend is required") {
		t.Errorf("error = %q, does not name the problem", err)
	}
	if called {
		t.Error("an empty backend reached the server; it must fail before the request")
	}
}

// TestModelsNonOKReturnsAPIError covers the statuses this endpoint documents
// (400 backend required, 404 unknown backend) plus the 500 the acceptance
// criteria call for.
//
// The 404 deserves note: it is what a backend with no configured gateway
// answers, which is an ordinary state rather than a fault. It still surfaces as
// an APIError — the client reports what happened and lets the caller decide —
// so the status must be recoverable from the error.
func TestModelsNonOKReturnsAPIError(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"backend required", http.StatusBadRequest, `{"ok":false,"error":"backend required"}`},
		{"unknown backend", http.StatusNotFound, `{"ok":false,"error":"unknown inference backend: nope"}`},
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

			got, err := newTestClient(t, server, "tok").Models(context.Background(), "nope")
			if err == nil {
				t.Fatalf("Models() error = nil, want *APIError for %d", tc.code)
			}
			if got.Backend != "" || got.Models != nil {
				t.Errorf("result = %+v, want the zero value on error", got)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.code)
			}
			if !strings.Contains(apiErr.Error(), http.StatusText(tc.code)) {
				t.Errorf("Error() = %q, does not name the status", apiErr.Error())
			}
			if !strings.Contains(apiErr.Error(), "/api/inference/models/nope") {
				t.Errorf("Error() = %q, does not name the path", apiErr.Error())
			}
		})
	}
}

// TestModelsMalformedBody: a 200 carrying something that is not this envelope
// is a decode failure, not an API error. A pane retries a transport blip and
// reports a malformed payload; conflating them hides one behind the other.
func TestModelsMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `["not","an","envelope"]`)
	}))
	defer server.Close()

	_, err := newTestClient(t, server, "tok").Models(context.Background(), "vllm")
	if err == nil {
		t.Fatal("Models() = nil, want a decode error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("decode failure surfaced as *APIError (%v); the response WAS a 200", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, does not name the decode failure", err)
	}
}

// TestSetAgentModelDecodesFixture covers the complete write contract: both
// inputs are escaped as individual path segments, the operation is a bodiless
// authenticated POST, and every published response field is decoded.
func TestSetAgentModelDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "model-set.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod, gotAuth, gotContentType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.EscapedPath(), r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").SetAgentModel(
		context.Background(), "review/team", "meta/llama-3.1",
	)
	if err != nil {
		t.Fatalf("SetAgentModel() = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/model/review%2Fteam/meta%2Fllama-3.1" {
		t.Errorf("escaped path = %q, want /api/model/review%%2Fteam/meta%%2Fllama-3.1", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if len(gotBody) != 0 {
		t.Errorf("request body = %q, want empty", gotBody)
	}
	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want unset for a bodiless POST", gotContentType)
	}

	want := ModelSetResult{
		Status: "model_set",
		Agent:  "review/team",
		Model:  "meta/llama-3.1",
	}
	if got != want {
		t.Errorf("SetAgentModel() = %+v, want %+v", got, want)
	}
}

// TestSetAgentModelRequiresPathParameters verifies caller mistakes fail before
// a round trip instead of producing a misleading route-level 404.
func TestSetAgentModelRequiresPathParameters(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	cases := []struct {
		name  string
		agent string
		model string
		want  string
	}{
		{name: "empty agent", model: "gpt-5", want: "agent is required"},
		{name: "empty model", agent: "scanner", want: "model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newTestClient(t, server, "tok").SetAgentModel(context.Background(), tc.agent, tc.model)
			if err == nil {
				t.Fatal("error = nil, want a local validation error")
			}
			if got != (ModelSetResult{}) {
				t.Errorf("result = %+v, want zero value", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
	if called {
		t.Error("an empty path parameter reached the server; both must fail locally")
	}
}

// TestSetAgentModelNonOKReturnsAPIError proves the client preserves the
// backend's model-validation explanation and still handles server failures
// through the shared typed error path.
func TestSetAgentModelNonOKReturnsAPIError(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{
			name: "model not available",
			code: http.StatusBadRequest,
			body: `{"ok":false,"error":"model meta/llama-3.1 is not available for backend codex"}`,
		},
		{name: "server error", code: http.StatusInternalServerError, body: `{"ok":false,"error":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			got, err := newTestClient(t, server, "tok").SetAgentModel(context.Background(), "scanner", "meta/llama-3.1")
			if err == nil {
				t.Fatalf("SetAgentModel() error = nil, want *APIError for %d", tc.code)
			}
			if got != (ModelSetResult{}) {
				t.Errorf("result = %+v, want zero value", got)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.code)
			}
			if apiErr.Method != http.MethodPost {
				t.Errorf("Method = %q, want POST", apiErr.Method)
			}
			if apiErr.Path != "/api/model/scanner/meta%2Fllama-3.1" {
				t.Errorf("Path = %q, want escaped model path", apiErr.Path)
			}
			if apiErr.Body != tc.body {
				t.Errorf("Body = %q, want %q", apiErr.Body, tc.body)
			}
		})
	}
}

// TestSetAgentModelMalformedBody distinguishes a malformed success response
// from a dashboard status error.
func TestSetAgentModelMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `not-json`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").SetAgentModel(context.Background(), "scanner", "gpt-5")
	if err == nil {
		t.Fatal("SetAgentModel() error = nil, want a decode error")
	}
	if got != (ModelSetResult{}) {
		t.Errorf("result = %+v, want zero value", got)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("decode failure surfaced as *APIError (%v); the response was a 2xx", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, does not name the decode failure", err)
	}
}
