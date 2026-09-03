package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// streamWait bounds how long a test waits for the stream request to arrive.
// Generous on purpose: this asserts a header, not a latency, and a tight bound
// would turn a loaded CI runner into a failing auth test.
const streamWait = 2 * time.Second

// newCredentialedClient points a Client at a fixture server with both
// credentials set explicitly, bypassing New() so no test depends on the
// developer's own HIVE_DASHBOARD_* environment.
func newCredentialedClient(t *testing.T, server *httptest.Server, token, cookie string) *Client {
	t.Helper()
	c := New()
	c.baseURL = server.URL
	c.token = token
	c.cookie = cookie
	return c
}

// TestAuthorizeSendsCookie is the core assertion of the session-auth lane: the
// value goes out as a Cookie header, verbatim.
//
// Verbatim matters. The dashboard's session lookup reads a NAMED cookie
// (hive_session) out of the header, and a hub reads its own, so a client that
// reformatted the operator's value — trimmed a name, re-encoded it, wrapped it
// — would produce a request that authenticates as nobody while looking correct
// in a log.
func TestAuthorizeSendsCookie(t *testing.T) {
	var gotCookie, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const cookie = "hive_session=abc123"
	if err := newCredentialedClient(t, server, "", cookie).Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}

	if gotCookie != cookie {
		t.Errorf("Cookie = %q, want %q", gotCookie, cookie)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty with no token configured", gotAuth)
	}
}

// TestAuthorizeSendsBothCredentials pins that a token and a cookie coexist.
//
// This is the deployment-agnostic property New() documents: the client cannot
// tell a shared-token spoke from a direct-route one, so it presents both and
// lets the server pick the lane it actually implements. Dropping either when
// the other is present would lock out exactly one kind of hive.
func TestAuthorizeSendsBothCredentials(t *testing.T) {
	var gotCookie, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newCredentialedClient(t, server, "s3cret", "hive_session=abc").Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}

	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cret")
	}
	if gotCookie != "hive_session=abc" {
		t.Errorf("Cookie = %q, want %q", gotCookie, "hive_session=abc")
	}
}

// TestAuthorizeOmitsEmptyCookie covers the unset case.
//
// An empty Cookie header is not the same as no Cookie header to every
// intermediary between here and the hive, and the token path already learned
// this lesson (TestHealthOmitsAuthorizationWithoutToken). Omission is the only
// behaviour that leaves an unconfigured client indistinguishable from the
// pre-session-auth one.
func TestAuthorizeOmitsEmptyCookie(t *testing.T) {
	sawCookieHeader := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawCookieHeader = r.Header["Cookie"]
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newCredentialedClient(t, server, "s3cret", "").Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}

	if sawCookieHeader {
		t.Error("request carried a Cookie header with no cookie configured")
	}
}

// TestStreamEventsSendsCookie pins that the stream authenticates the same way
// the polls do.
//
// The two request paths are separate functions, and this is the test that stops
// them drifting. A cookie honoured by the polls but not the stream would fill
// every pane while the header read "not connected" — a screen that blames the
// hive for a client-side omission.
func TestStreamEventsSendsCookie(t *testing.T) {
	gotCookie := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie <- r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, _ := newCredentialedClient(t, server, "", "hive_session=xyz").StreamEvents(ctx)

	select {
	case got := <-gotCookie:
		if got != "hive_session=xyz" {
			t.Errorf("Cookie = %q, want %q", got, "hive_session=xyz")
		}
	case <-time.After(streamWait):
		t.Fatal("stream request never reached the server")
	}

	cancel()
	for range events { //nolint:revive // drain so the goroutine finishes before the server closes
	}
}

// TestIsUnauthorizedClassifies pins the 401/403 split preflight depends on.
//
// Collapsing them would break the TUI in one direction or the other: treating
// 403 as fatal would refuse to start for a viewer whose read-only session works
// for most of the screen, and treating 401 as survivable puts the operator back
// in front of four panes that will never fill.
func TestIsUnauthorizedClassifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"401", &APIError{StatusCode: http.StatusUnauthorized}, true},
		{"403", &APIError{StatusCode: http.StatusForbidden}, false},
		{"500", &APIError{StatusCode: http.StatusInternalServerError}, false},
		{"nil", nil, false},
		{"not an APIError", errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnauthorized(tc.err); got != tc.want {
				t.Errorf("IsUnauthorized(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCheckCredentialsProbesStatus pins which endpoint the probe reads.
//
// The path is the assertion, not an implementation detail: the probe is only
// meaningful because /api/status is the same authenticated read the Governor
// pane makes on every cycle. Point it at something the app never calls and a
// passing probe stops predicting a frame that fills.
func TestCheckCredentialsProbesStatus(t *testing.T) {
	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		// A body the probe must not need to understand.
		_, _ = w.Write([]byte(`{"mode":"BUSY"}`))
	}))
	defer server.Close()

	if err := newCredentialedClient(t, server, "t", "").CheckCredentials(context.Background()); err != nil {
		t.Fatalf("CheckCredentials() = %v, want nil", err)
	}

	if gotPath != "/api/status" {
		t.Errorf("path = %q, want /api/status", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
}

// TestCheckCredentialsReturnsUnwrappedAPIError pins that the caller can still
// classify the failure. preflight's whole behaviour hangs off IsUnauthorized
// matching, so an error wrapped into a string here would silently turn every
// 401 into "some error" and start the TUI anyway.
func TestCheckCredentialsReturnsUnwrappedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	err := newCredentialedClient(t, server, "wrong", "").CheckCredentials(context.Background())
	if !IsUnauthorized(err) {
		t.Fatalf("CheckCredentials() = %v, want an APIError carrying 401", err)
	}
}
