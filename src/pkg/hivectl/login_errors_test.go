package hivectl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This file covers the flow's failure exits — the paths an operator hits when
// the hive, the network, or the cache misbehaves. The happy paths live in
// login_test.go; these tests exist because a login tool's error behaviour IS
// its behaviour for the operator having the worst day.

// TestDeviceLoginStartFailure pins that a start the server refuses ends the
// flow immediately with the API error — no prompt, no polling against a flow
// that never began.
func TestDeviceLoginStartFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ghAuthStartPath {
			t.Errorf("unexpected request to %s after a failed start", r.URL.Path)
		}
		http.Error(w, `{"error":"device flow start failed"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	prompted := false
	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{
		Prompt: func(DeviceLoginPrompt) { prompted = true },
		Sleep:  noSleep(&sleeps),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("DeviceLogin() = %v, want the server's 500 as an APIError", err)
	}
	if prompted {
		t.Error("operator was prompted for a flow that never started")
	}
}

// TestDeviceLoginStartWithoutCode pins the two malformed-start exits: a 200
// that is not the device-flow shape at all, and one that is but carries no
// code. Both mean "this is not a hive dashboard (or not one that can log you
// in)" and must say so rather than prompting the operator with an empty code.
func TestDeviceLoginStartWithoutCode(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"valid JSON, no code", `{"interval":5}`, "no user code"},
		{"not the JSON shape", `"just a string"`, "decode device-flow start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			var sleeps []time.Duration
			_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DeviceLogin() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestDeviceLoginPollServerError pins the non-400 HTTP failure exit on poll: a
// 500 surfaces as an APIError, not as a conflict and not as a retry — the
// flow's poll loop trusts terminal answers.
func TestDeviceLoginPollServerError(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			func(w http.ResponseWriter) {
				http.Error(w, `{"error":"session store unavailable"}`, http.StatusInternalServerError)
			},
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("DeviceLogin() = %v, want the poll's 500 as an APIError", err)
	}
	if errors.Is(err, ErrDeviceFlowConflict) {
		t.Error("a 500 classified as the device-flow conflict, which is 400-only")
	}
}

// TestDeviceLoginPollRaceWithPlainBody pins apiErrorMessage's fallback: a 400
// whose body is not the {"error": ...} shape still classifies as the conflict
// and carries the raw body rather than dropping the server's words.
func TestDeviceLoginPollRaceWithPlainBody(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			func(w http.ResponseWriter) {
				http.Error(w, "no device flow in progress", http.StatusBadRequest)
			},
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if !errors.Is(err, ErrDeviceFlowConflict) {
		t.Fatalf("DeviceLogin() = %v, want ErrDeviceFlowConflict", err)
	}
	if !strings.Contains(err.Error(), "no device flow in progress") {
		t.Errorf("error %q dropped the server's plain-text body", err)
	}
}

// TestDeviceLoginPollMalformedJSON pins the decode exit: a 200 poll body that
// is not JSON is an error naming the poll, not a silent classification into
// some default status.
func TestDeviceLoginPollMalformedJSON(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>proxy error page</html>")) },
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err == nil || !strings.Contains(err.Error(), "decode device-flow poll") {
		t.Fatalf("DeviceLogin() = %v, want a poll decode error", err)
	}
}

// TestDeviceLoginUnexpectedStatus pins the default branch: a status this
// client does not know is terminal and named, because looping on it would be
// the {status:"error"} spin bug wearing a new status string.
func TestDeviceLoginUnexpectedStatus(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"telepathic"}`),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err == nil || !strings.Contains(err.Error(), "telepathic") {
		t.Fatalf("DeviceLogin() = %v, want the unexpected status named", err)
	}
	if fixture.pollCalls != 1 {
		t.Errorf("polls = %d, want exactly 1 — an unknown status must not be retried", fixture.pollCalls)
	}
}

// TestDeviceLoginErrorWithoutMessage pins the empty-explanation fallback: a
// terminal {status:"error"} with no error text still produces a sentence, not
// `login failed: `.
func TestDeviceLoginErrorWithoutMessage(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"error"}`),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err == nil || !strings.Contains(err.Error(), "unspecified login error") {
		t.Fatalf("DeviceLogin() = %v, want the unspecified-error fallback", err)
	}
}

// TestDeviceLoginCancelDuringWait pins ctrl+c behaviour: a cancellation
// surfacing from the between-polls wait ends the flow with the context's
// error and no further poll — the operator's interrupt must not be followed
// by another request against the hive.
func TestDeviceLoginCancelDuringWait(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		// No polls scripted: cancellation must stop the flow before any.
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	cancelledSleep := func(_ context.Context, _ time.Duration) error { return context.Canceled }
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: cancelledSleep})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DeviceLogin() = %v, want context.Canceled", err)
	}
	if fixture.pollCalls != 0 {
		t.Errorf("polls = %d after cancellation, want 0", fixture.pollCalls)
	}
}

// TestSleepContext covers the production sleeper directly: a non-positive
// duration returns immediately, a short one elapses, and cancellation
// interrupts a long one without waiting it out.
func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Errorf("sleepContext(0) = %v, want nil", err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepContext(1ms) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepContext(cancelled, 1h) = %v, want context.Canceled", err)
	}
}

// TestDeviceLoginUnreachableHive pins the connection-failure exit on poll: a
// hive that vanishes mid-flow surfaces as a ConnectionError, the same
// classification every other hivectl request uses (exit code 4, not 1).
func TestDeviceLoginUnreachableHive(t *testing.T) {
	fixture := &deviceFlowFixture{t: t, startInterval: 5, startExpiresIn: 900}
	server := httptest.NewServer(fixture.handler())

	client := loginClient(t, server.URL)
	// The hive goes away after start succeeds but before the first poll.
	sleepThenKill := func(_ context.Context, _ time.Duration) error {
		server.Close()
		return nil
	}
	_, err := client.DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: sleepThenKill})
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("DeviceLogin() = %v, want a ConnectionError", err)
	}
}

// TestLoginResultExpiryFromCookieExpires covers the Expires-attribute branch:
// a server that dates the session cookie instead of using Max-Age still bounds
// the cache entry by the server's own date.
func TestLoginResultExpiryFromCookieExpires(t *testing.T) {
	stamp := time.Now().Add(72 * time.Hour).Truncate(time.Second).UTC()
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"complete","username":"octocat"}`,
				&http.Cookie{Name: "hive_session", Value: "dated", Expires: stamp}),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	result, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err != nil {
		t.Fatalf("DeviceLogin() = %v", err)
	}
	if !result.ExpiresAt.Equal(stamp) {
		t.Errorf("ExpiresAt = %v, want the cookie's Expires %v", result.ExpiresAt, stamp)
	}
}

// TestLoginResultSkipsClearedCookies pins the tombstone filter: a Set-Cookie
// that DELETES a cookie (empty value / negative Max-Age, the shape
// clearSessionCookie emits) must not ride into the cached credential, where it
// would be presented on every future request.
func TestLoginResultSkipsClearedCookies(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"complete","username":"octocat"}`,
				&http.Cookie{Name: "stale_cookie", Value: "", MaxAge: -1},
				&http.Cookie{Name: "hive_session", Value: "fresh", MaxAge: 3600}),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	result, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err != nil {
		t.Fatalf("DeviceLogin() = %v", err)
	}
	if result.Cookie != "hive_session=fresh" {
		t.Errorf("Cookie = %q, want only the live cookie", result.Cookie)
	}
}

// TestClientLogout covers the Logout method: the right endpoint, the right
// method, and the configured session presented so the server ends THAT
// session.
func TestClientLogout(t *testing.T) {
	var gotPath, gotMethod, gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotCookie = r.URL.Path, r.Method, r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"logged_out"}`))
	}))
	defer server.Close()

	client := loginClient(t, server.URL)
	client.SetSessionCookie("hive_session=abc")
	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("Logout() = %v", err)
	}
	if gotPath != ghAuthLogoutPath || gotMethod != http.MethodPost {
		t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, ghAuthLogoutPath)
	}
	if gotCookie != "hive_session=abc" {
		t.Errorf("Cookie = %q, want the session being ended", gotCookie)
	}
}

// TestClientLogoutSurfacesFailure pins that a refused logout is an error the
// command layer can report — the local cache is cleared regardless, but the
// operator is told the server did not confirm.
func TestClientLogoutSurfacesFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"failed to remove persisted GitHub credentials"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	err := loginClient(t, server.URL).Logout(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Logout() = %v, want the server's 500", err)
	}
}

// TestDefaultSessionStorePath pins where the cache lives: XDG_CONFIG_HOME when
// set — explicitly honoured on every platform, see the function doc — and the
// platform config dir otherwise.
func TestDefaultSessionStorePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	store, err := DefaultSessionStore()
	if err != nil {
		t.Fatalf("DefaultSessionStore() = %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hive", "sessions.json")
	if store.Path() != want {
		t.Errorf("Path() = %q, want %q", store.Path(), want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	store, err = DefaultSessionStore()
	if err != nil {
		t.Fatalf("DefaultSessionStore() without XDG = %v", err)
	}
	if !strings.HasSuffix(store.Path(), filepath.Join("hive", "sessions.json")) {
		t.Errorf("Path() = %q, want a hive/sessions.json under the platform config dir", store.Path())
	}
}

// TestDefaultSessionStoreNoHome pins the no-config-dir exit: with neither
// XDG_CONFIG_HOME nor HOME there is nowhere safe to put a credential, and that
// must be an error naming the cache, not a panic or a file in the working
// directory.
func TestDefaultSessionStoreNoHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserConfigDir does not depend on HOME on windows")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if _, err := DefaultSessionStore(); err == nil {
		t.Fatal("DefaultSessionStore() = nil error with no HOME and no XDG_CONFIG_HOME, want an error")
	}
}
