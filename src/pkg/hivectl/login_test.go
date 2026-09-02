package hivectl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deviceFlowFixture is an httptest stand-in for the dashboard's gh-user-auth
// surface: /start answers with codes, /poll walks a scripted sequence of
// responses. Per the hivectl tui epic's testing convention there is no GitHub,
// no browser, and no network — the fixture IS the hive.
type deviceFlowFixture struct {
	t *testing.T
	// polls is the scripted sequence; each call to /poll shifts one off.
	polls []func(w http.ResponseWriter)
	// startInterval and startExpiresIn parameterize the /start response.
	startInterval  int
	startExpiresIn int
	pollCalls      int
}

func (f *deviceFlowFixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ghAuthStartPath:
			if r.Method != http.MethodPost {
				f.t.Errorf("start method = %s, want POST", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device",` +
				`"expires_in":` + strconv.Itoa(f.startExpiresIn) + `,"interval":` + strconv.Itoa(f.startInterval) + `}`))
		case ghAuthPollPath:
			if len(f.polls) == 0 {
				f.t.Errorf("unexpected extra poll #%d", f.pollCalls+1)
				http.Error(w, `{"error":"unexpected poll"}`, http.StatusInternalServerError)
				return
			}
			f.pollCalls++
			next := f.polls[0]
			f.polls = f.polls[1:]
			next(w)
		default:
			f.t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func pollJSON(body string, cookies ...*http.Cookie) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for _, ck := range cookies {
			http.SetCookie(w, ck)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func loginClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := NewClient(url, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// noSleep satisfies DeviceLoginOptions.Sleep while recording what the loop
// asked for, so interval assertions never make a test actually wait.
func noSleep(record *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*record = append(*record, d)
		return nil
	}
}

// TestDeviceLoginHappyPath drives the whole flow: start → prompt → pending →
// complete, with the session arriving as Set-Cookie on the POLL response —
// which is where the server actually puts it; there is no separate exchange.
func TestDeviceLoginHappyPath(t *testing.T) {
	fixture := &deviceFlowFixture{
		t:              t,
		startInterval:  5,
		startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"pending"}`),
			pollJSON(`{"status":"complete","username":"octocat","avatar_url":"https://example.invalid/a.png"}`,
				&http.Cookie{Name: "hive_session", Value: "s3ss10n", MaxAge: 3600},
				&http.Cookie{Name: "hive_terminal_assertion", Value: "assert1", MaxAge: 600}),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var prompts []DeviceLoginPrompt
	var sleeps []time.Duration
	start := time.Now()
	result, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{
		Prompt: func(p DeviceLoginPrompt) { prompts = append(prompts, p) },
		Sleep:  noSleep(&sleeps),
	})
	if err != nil {
		t.Fatalf("DeviceLogin() = %v", err)
	}

	if len(prompts) != 1 || prompts[0].UserCode != "WDJB-MJHT" || prompts[0].VerificationURI != "https://github.com/login/device" {
		t.Errorf("prompts = %+v, want one with the fixture's code and URI", prompts)
	}
	if result.Username != "octocat" {
		t.Errorf("Username = %q, want octocat", result.Username)
	}
	// Both cookies travel, joined the way a browser would send them — the
	// HIVE_DASHBOARD_COOKIE convention from #5649, which is what lets the TUI
	// and every subcommand consume this value unchanged.
	if result.Cookie != "hive_session=s3ss10n; hive_terminal_assertion=assert1" {
		t.Errorf("Cookie = %q, want both cookies joined with '; '", result.Cookie)
	}
	// Expiry follows the hive_session Max-Age (1h), not the fallback and not
	// the shorter-lived assertion cookie.
	wantExpiry := start.Add(time.Hour)
	if result.ExpiresAt.Before(wantExpiry.Add(-time.Minute)) || result.ExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ~%v", result.ExpiresAt, wantExpiry)
	}
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second {
		t.Errorf("sleeps = %v, want two sleeps at the server's 5s interval", sleeps)
	}
}

// TestDeviceLoginDeniedStopsWithServerReason pins the trap the issue calls
// out: an allowlist rejection arrives as {status:"error"} inside a 200. The
// loop must stop on it (no spin) and surface the SERVER'S explanation, which
// names the account and what to do, not a generic failure.
func TestDeviceLoginDeniedStopsWithServerReason(t *testing.T) {
	const denial = "your GitHub account (mallory) is not authorized to access this hive. Contact the hive owner to request access."
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"error","error":"` + denial + `"}`),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err == nil || !strings.Contains(err.Error(), denial) {
		t.Fatalf("DeviceLogin() = %v, want the server's denial verbatim", err)
	}
	if fixture.pollCalls != 1 {
		t.Errorf("polls = %d, want exactly 1 — a terminal error must not be retried", fixture.pollCalls)
	}
}

// TestDeviceLoginLostRaceIsNamed covers the one-flow-per-spoke constraint: a
// spoke holds a single device-flow state, so a concurrent operator's start
// clobbers ours and poll answers a bare 400. That must classify as
// ErrDeviceFlowConflict with actionable text, not surface as a mystery status.
func TestDeviceLoginLostRaceIsNamed(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			func(w http.ResponseWriter) {
				http.Error(w, `{"error":"no device flow in progress — call /api/gh-user-auth/start first"}`, http.StatusBadRequest)
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
	if !strings.Contains(err.Error(), "hivectl login") {
		t.Errorf("error %q does not tell the operator to retry with 'hivectl login'", err)
	}
}

// TestDeviceLoginSlowDownBacksOff pins the GitHub device-flow contract the
// server relays: every slow_down adds five seconds to the interval, so an
// operator's login never rate-limits the hive's own OAuth app.
func TestDeviceLoginSlowDownBacksOff(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"slow_down"}`),
			pollJSON(`{"status":"complete","username":"octocat"}`,
				&http.Cookie{Name: "hive_session", Value: "abc", MaxAge: 3600}),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	if _, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)}); err != nil {
		t.Fatalf("DeviceLogin() = %v", err)
	}
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second || sleeps[1] != 10*time.Second {
		t.Errorf("sleeps = %v, want [5s 10s]", sleeps)
	}
}

// TestDeviceLoginExpiresWithRemedy pins the deadline: a code the operator
// never approves must end the flow with advice to run `hivectl login` again,
// not poll forever.
func TestDeviceLoginExpiresWithRemedy(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 600,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"pending"}`),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	// A fake clock that jumps past the 600s expiry after the first poll.
	base := time.Now()
	calls := 0
	now := func() time.Time {
		calls++
		if calls > 2 {
			return base.Add(time.Hour)
		}
		return base
	}
	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps), Now: now})
	if err == nil || !strings.Contains(err.Error(), "hivectl login") {
		t.Fatalf("DeviceLogin() = %v, want an expiry error naming 'hivectl login'", err)
	}
}

// TestDeviceLoginCompleteWithoutCookieFails pins that a "complete" carrying no
// Set-Cookie is an error, not a silently empty cache entry that would turn
// every later command's 401 into a mystery.
func TestDeviceLoginCompleteWithoutCookieFails(t *testing.T) {
	fixture := &deviceFlowFixture{
		t: t, startInterval: 5, startExpiresIn: 900,
		polls: []func(http.ResponseWriter){
			pollJSON(`{"status":"complete","username":"octocat"}`),
		},
	}
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var sleeps []time.Duration
	_, err := loginClient(t, server.URL).DeviceLogin(context.Background(), DeviceLoginOptions{Sleep: noSleep(&sleeps)})
	if err == nil || !strings.Contains(err.Error(), "session cookie") {
		t.Fatalf("DeviceLogin() = %v, want an error about the missing session cookie", err)
	}
}

// TestClientSendsSessionCookieVerbatim pins the session lane on the plain API
// client: the configured header value goes out unmodified, alongside the
// token when both are set — which lane the hive honours is a deployment
// property the client cannot see, so it presents both.
func TestClientSendsSessionCookieVerbatim(t *testing.T) {
	var gotCookie, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "s3cret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.SetSessionCookie("hive_session=abc; hive_terminal_assertion=xyz")
	if _, err := client.Do(context.Background(), http.MethodGet, "/api/status", nil, nil); err != nil {
		t.Fatalf("Do() = %v", err)
	}
	if gotCookie != "hive_session=abc; hive_terminal_assertion=xyz" {
		t.Errorf("Cookie = %q, want the configured value verbatim", gotCookie)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the token lane untouched", gotAuth)
	}
}

// TestClientOmitsEmptyCookie pins omission over an empty header: an empty
// Cookie header is not the same as no Cookie header to every intermediary
// between here and the hive — the same lesson the token lane already encodes.
func TestClientOmitsEmptyCookie(t *testing.T) {
	sawCookieHeader := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawCookieHeader = r.Header["Cookie"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "s3cret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), http.MethodGet, "/api/status", nil, nil); err != nil {
		t.Fatalf("Do() = %v", err)
	}
	if sawCookieHeader {
		t.Error("request carried a Cookie header with no session configured")
	}
}

// TestClientLoginHintOn401Only pins where the "run hivectl login" advice may
// appear: on 401 — no usable credential, where logging in again is the fix —
// and never on 403, which is a WORKING session whose role is too narrow;
// sending that operator back through login would change nothing.
func TestClientLoginHintOn401Only(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantHint bool
	}{
		{"401 carries the hint", http.StatusUnauthorized, true},
		{"403 does not", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"nope"}`, tc.status)
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "", 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client.SetSessionCookie("hive_session=stale")
			client.SetLoginHint("run 'hivectl login' to obtain a new one")
			_, err = client.Do(context.Background(), http.MethodGet, "/api/status", nil, nil)
			if err == nil {
				t.Fatal("Do() = nil, want an APIError")
			}
			if got := strings.Contains(err.Error(), "hivectl login"); got != tc.wantHint {
				t.Errorf("error %q mentions 'hivectl login' = %v, want %v", err, got, tc.wantHint)
			}
		})
	}
}
