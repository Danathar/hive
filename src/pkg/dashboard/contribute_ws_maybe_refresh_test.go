package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests cover the heartbeat-driven maybeRefreshToken orchestrator
// (contribute_ws.go), the glue between tokenRefreshDue, mintScopedToken, and
// sendTokenRefresh. Its helpers are each unit-tested elsewhere
// (contribute_ws_token_refresh_test.go, contribute_ws_sec_c4_h2_test.go), but
// the orchestrator's own branches — not-due early return, mint failure, empty
// token with no App auth, send failure — were not, and they are the
// auth-critical retry policy: on any failure the relay's EXISTING token must be
// left in place and tokenMintedAt must NOT advance, so the very next heartbeat
// retries the refresh instead of silently letting the credential expire at
// wsTokenTTL (#2393 item 2).

// refreshConn builds a ContributorConnection in the mid-task state
// maybeRefreshToken sees from the heartbeat loop. A nil ws is a deliberate
// tripwire in the no-send cases: any branch that wrongly reaches
// sendTokenRefresh panics instead of passing silently.
func refreshConn(ws *websocket.Conn, mintedAt time.Time) *ContributorConnection {
	return &ContributorConnection{
		ws:            ws,
		profile:       &ContributorProfile{TrustTier: "contributor", GitHubUsername: "refresh-user"},
		currentTask:   &WSTaskAssign{TaskID: "t-refresh", Repo: "o/r", Number: 7},
		tokenMintedAt: mintedAt,
	}
}

// wsPair stands up a real server/client websocket pair so a delivered
// token_refresh is an actual frame the client can read back and inspect.
func wsPair(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()
	connReady := make(chan struct{})
	handlerDone := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn = c
		close(connReady)
		// Keep the handler alive until the test is done with the conn; released
		// by the cleanup below (LIFO: runs before srv.Close) rather than a timer.
		<-handlerDone
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(handlerDone) })

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	<-connReady
	return serverConn, client
}

// TestMaybeRefreshToken_NotDueIsANoOp: before wsTokenRefreshPeriod has elapsed
// the heartbeat must not mint or send anything. The nil ws panics if a send is
// wrongly attempted, and tokenMintedAt must be left exactly as it was so the
// refresh schedule is not perturbed.
func TestMaybeRefreshToken_NotDueIsANoOp(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_should_never_mint")}
	hub.server = s

	minted := time.Now().Add(-wsTokenRefreshPeriod / 2)
	conn := refreshConn(nil, minted)

	hub.maybeRefreshToken(conn)

	conn.mu.Lock()
	got := conn.tokenMintedAt
	conn.mu.Unlock()
	if !got.Equal(minted) {
		t.Fatalf("tokenMintedAt moved on a not-due heartbeat: %s, want %s", got, minted)
	}
}

// TestMaybeRefreshToken_MintFailureRetriesNextHeartbeat: when the mint fails
// (#2436-style App API error) NO CREDENTIAL may be sent — the relay keeps its
// existing token — and tokenMintedAt must NOT advance, so tokenRefreshDue still
// reports due on the next heartbeat and the refresh is retried rather than
// abandoned for the life of the task.
//
// As of #5447 this path DOES write one frame: an advisory token_refresh_failed
// carrying no token material (asserted below and, for its content, in
// TestMaybeRefreshToken_MintFailureNotifiesRelay). The connection therefore now
// needs a real socket rather than the nil-ws tripwire the other no-send cases
// still use; the retry policy this test guards is unchanged.
func TestMaybeRefreshToken_MintFailureRetriesNextHeartbeat(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newFailingAppAuth(t)}
	hub.server = s

	minted := time.Now().Add(-wsTokenRefreshPeriod - time.Minute)
	server, client := wsPair(t)
	conn := refreshConn(server, minted)

	hub.maybeRefreshToken(conn)

	// Whatever is written on a failed mint, it must never be a credential.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read after failed mint: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh_failed" {
		t.Fatalf("type = %v, want token_refresh_failed", wire["type"])
	}
	if _, ok := wire["github_token"]; ok {
		t.Fatalf("a failed mint must never put token material on the wire: %s", data)
	}

	conn.mu.Lock()
	got := conn.tokenMintedAt
	conn.mu.Unlock()
	if !got.Equal(minted) {
		t.Fatalf("tokenMintedAt moved after a failed mint: %s, want %s (retry must stay armed)", got, minted)
	}
	if _, _, due := tokenRefreshDue(conn, time.Now()); !due {
		t.Fatalf("after a failed mint, refresh must still be due on the next heartbeat")
	}
}

// TestMaybeRefreshToken_EmptyTokenLeavesRelayToken: with no App auth
// mintScopedToken deliberately returns ("", nil) rather than a full-credential
// fallback (C4). maybeRefreshToken must treat that as "nothing to push": no
// send, and tokenMintedAt unchanged so a later mint success can still refresh.
func TestMaybeRefreshToken_EmptyTokenLeavesRelayToken(t *testing.T) {
	// No hub.server at all: the mintScopedToken no-App-auth branch.
	hub := &ContributeWSHub{logger: slog.Default()}

	minted := time.Now().Add(-wsTokenRefreshPeriod - time.Minute)
	conn := refreshConn(nil, minted) // nil ws: a send attempt would panic

	hub.maybeRefreshToken(conn)

	conn.mu.Lock()
	got := conn.tokenMintedAt
	conn.mu.Unlock()
	if !got.Equal(minted) {
		t.Fatalf("tokenMintedAt moved with no App auth: %s, want %s", got, minted)
	}
	if _, _, due := tokenRefreshDue(conn, time.Now()); !due {
		t.Fatalf("with no App auth, refresh must remain due so a later mint can still fire")
	}
}

// TestMaybeRefreshToken_SuccessPushesTokenRefresh: the happy path end to end
// through the orchestrator — due, mint succeeds, and the relay receives a
// token_refresh frame carrying the new scoped token, after which
// tokenMintedAt is advanced so the next refresh is scheduled off the new token.
func TestMaybeRefreshToken_SuccessPushesTokenRefresh(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_heartbeat_refreshed")}
	hub.server = s

	serverConn, client := wsPair(t)

	before := time.Now()
	minted := before.Add(-wsTokenRefreshPeriod - time.Minute)
	conn := refreshConn(serverConn, minted)

	hub.maybeRefreshToken(conn)

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh" {
		t.Fatalf("type = %v, want token_refresh", wire["type"])
	}
	if wire["github_token"] != "ghs_heartbeat_refreshed" {
		t.Fatalf("github_token = %v, want the freshly minted token", wire["github_token"])
	}

	conn.mu.Lock()
	got := conn.tokenMintedAt
	conn.mu.Unlock()
	if !got.After(before.Add(-time.Second)) {
		t.Fatalf("tokenMintedAt not advanced after a successful refresh: %s", got)
	}
	if _, _, due := tokenRefreshDue(conn, time.Now()); due {
		t.Fatalf("immediately after a successful refresh, the next refresh must not already be due")
	}
}

// TestMaybeRefreshToken_SendFailureKeepsRetryArmed: a mint that succeeds but a
// push that fails (relay socket already gone) must not advance tokenMintedAt —
// sendTokenRefresh records the mint time only AFTER a successful write — so if
// the connection somehow survives, the next heartbeat retries the refresh.
func TestMaybeRefreshToken_SendFailureKeepsRetryArmed(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_never_delivered")}
	hub.server = s

	serverConn, client := wsPair(t)
	// Kill the transport before the refresh fires so c.send fails.
	serverConn.Close()
	client.Close()

	minted := time.Now().Add(-wsTokenRefreshPeriod - time.Minute)
	conn := refreshConn(serverConn, minted)

	hub.maybeRefreshToken(conn)

	conn.mu.Lock()
	got := conn.tokenMintedAt
	conn.mu.Unlock()
	if !got.Equal(minted) {
		t.Fatalf("tokenMintedAt moved after a failed send: %s, want %s (retry must stay armed)", got, minted)
	}
	if _, _, due := tokenRefreshDue(conn, time.Now()); !due {
		t.Fatalf("after a failed send, refresh must still be due on the next heartbeat")
	}
}

// TestMaybeRefreshToken_MintFailureNotifiesRelay is the hub half of #5447.
//
// Before it, a failed re-mint was recorded ONLY in the hub's log. The relay was
// told nothing, so its first evidence that the credential it holds had gone
// stale was a push failing roughly an hour into a long task, which the agent
// reported as a generic auth error — the misleading-symptom class of #5343.
//
// This asserts the OBSERVABLE consequence (#5388): a real frame, of a named
// type, carrying a reason and no token material. Asserting only that the hub
// "handles" a mint failure would pass against the old silent code.
func TestMaybeRefreshToken_MintFailureNotifiesRelay(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newFailingAppAuth(t)}
	hub.server = s

	server, client := wsPair(t)
	conn := refreshConn(server, time.Now().Add(-wsTokenRefreshPeriod-time.Minute))

	hub.maybeRefreshToken(conn)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("the relay was never told the re-mint failed: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh_failed" {
		t.Fatalf("type = %v, want token_refresh_failed", wire["type"])
	}
	reason, _ := wire["reason"].(string)
	if reason == "" {
		t.Fatalf("token_refresh_failed carried no reason: %s", data)
	}
	// The reason is a fixed, caller-supplied string. The mint error itself can
	// quote GitHub App responses, and that must not cross to a
	// contributor-controlled process.
	if strings.Contains(strings.ToLower(reason), "ghs_") || strings.Contains(reason, "token ") {
		t.Fatalf("reason looks like it leaked auth detail: %q", reason)
	}
	if _, ok := wire["github_token"]; ok {
		t.Fatalf("token material on a failure notice: %s", data)
	}
}

// TestResumeTaskToken_MintFailureNotifiesRelay: the reconnect path must be as
// loud as the heartbeat one. A resume whose mint fails leaves the relay holding
// a credential minted before the disconnect, with refresh un-armed — exactly the
// state in which a later push fails for no visible reason (#5447).
func TestResumeTaskToken_MintFailureNotifiesRelay(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{GHAppAuth: newFailingAppAuth(t)}
	hub.server = s

	server, client := wsPair(t)
	conn := refreshConn(server, time.Time{})

	hub.resumeTaskToken(conn, &taskLease{taskID: "t-resume", repo: "o/r", number: 7, tier: "contributor", gen: 1})

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("a failed resume mint told the relay nothing: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "token_refresh_failed" {
		t.Fatalf("type = %v, want token_refresh_failed", wire["type"])
	}
	if _, ok := wire["github_token"]; ok {
		t.Fatalf("token material on a failure notice: %s", data)
	}

	// The lenient retry policy is unchanged: a failed resume mint must leave
	// tokenMintedAt zero so a later resume or heartbeat can still arm refresh.
	conn.mu.Lock()
	minted := conn.tokenMintedAt
	conn.mu.Unlock()
	if !minted.IsZero() {
		t.Fatalf("tokenMintedAt = %s, want zero after a failed resume mint", minted)
	}
}

// TestServerCapabilitiesAdvertisesTokenRefreshFailed: the notice is only useful
// if a client can learn the hub sends it without probing (#2567's contract).
func TestServerCapabilitiesAdvertisesTokenRefreshFailed(t *testing.T) {
	caps := serverCapabilities()
	for _, c := range caps {
		if c == capTokenRefreshFailed {
			return
		}
	}
	t.Fatalf("serverCapabilities() = %v, missing %q", caps, capTokenRefreshFailed)
}
