package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_protocol_ping_test.go pins the KEEPALIVE half of
// kubestellar/hive#5090.
//
// THE DEFECT: the contributor keepalive was implemented entirely in application
// JSON. The hub sent {"type":"ping"} as an ordinary TEXT frame and the relay
// answered {"type":"pong"}; websocket.PingMessage appeared nowhere in this
// package and the relay never called ws.ping(). No WebSocket PROTOCOL-level
// control frame was ever exchanged.
//
// That is invisible to the two endpoints and decisive to everything between
// them. The hosted spokes sit behind ingress-nginx and an OCI load balancer,
// and an L7 proxy may score tunnel idleness on control-frame traffic alone —
// application payload can be a long unidirectional stream that proves nothing
// about liveness. Under such a proxy a socket carrying a text frame every 30s
// is still "idle" and gets reaped on the idle timer WITHOUT a Close frame, so
// the peer observes 1006 with an empty reason and neither side logs anything.
//
// That is exactly the signature #5090 measured: after #5107 the hub provably
// sends Close frames on every hangup of its own, yet every observed flap
// carried no frame at all. The cut was below the application, and the
// application was giving the intermediary nothing to hold onto.
//
// These tests assert on the CONTROL FRAME, not on the JSON — the JSON ping was
// present the whole time and is not what was broken.

// TestWS_HubSendsProtocolPingControlFrame is the core assertion: the hub's
// keepalive must reach the wire as a real Ping control frame (opcode 0x9).
//
// It calls writeProtocolPing directly rather than waiting on heartbeatLoop,
// because wsHeartbeatInterval is 30 seconds and a test may not sleep for it.
// heartbeatLoop's call to this same helper is a one-line delegation; what needed
// pinning is that the helper puts a control frame on the wire at all.
func TestWS_HubSendsProtocolPingControlFrame(t *testing.T) {
	var seen struct {
		sync.Mutex
		pings []string
	}
	gotPing := make(chan struct{}, 1)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := writeProtocolPing(conn); err != nil {
			t.Errorf("writeProtocolPing: %v", err)
		}
		// Keep the handler alive so the client can read the control frame.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetPingHandler(func(payload string) error {
		seen.Lock()
		seen.pings = append(seen.pings, payload)
		seen.Unlock()
		select {
		case gotPing <- struct{}{}:
		default:
		}
		// Answer as a conforming client would, which is what gorilla's default
		// handler does; doing it explicitly keeps this test honest about the
		// round trip rather than only the outbound leg.
		return conn.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second))
	})

	// Control frames are delivered to their handler from inside ReadMessage, so
	// the client must be reading for the ping to be observed at all.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-gotPing:
	case <-time.After(5 * time.Second):
		t.Fatal("no WebSocket Ping control frame arrived — the keepalive is still " +
			"JSON-only, so an idle-timing proxy sees a dead tunnel and reaps it " +
			"frameless (1006), which is the #5090 flap")
	}

	seen.Lock()
	n := len(seen.pings)
	seen.Unlock()
	if n == 0 {
		t.Fatal("ping handler recorded nothing")
	}
}

// TestWS_ProtocolPongRefreshesLiveness proves the reverse leg: a PROTOCOL-level
// Pong must count as liveness for the heartbeat sweep.
//
// This matters because the hub now pings with a control frame, and any
// conforming WebSocket client answers that with a control-frame Pong
// automatically — with no relay code at all. If the hub only credited the JSON
// {"type":"pong"}, such a client would be declared stale after
// wsHeartbeatTimeout and hung up: the fix would have manufactured a NEW flap.
func TestWS_ProtocolPongRefreshesLiveness(t *testing.T) {
	c := &ContributorConnection{
		// Deliberately stale: older than wsHeartbeatTimeout, so the heartbeat
		// sweep would close this connection if nothing refreshed it.
		lastPong: time.Now().Add(-2 * wsHeartbeatTimeout),
	}

	if time.Since(c.lastPong) <= wsHeartbeatTimeout {
		t.Fatal("test setup is wrong: the connection must start stale")
	}

	// The handler the hub installs on registration, applied here in isolation.
	handler := func(string) error {
		c.mu.Lock()
		c.lastPong = time.Now()
		c.mu.Unlock()
		return nil
	}
	if err := handler(""); err != nil {
		t.Fatalf("pong handler returned an error: %v", err)
	}

	c.mu.Lock()
	stale := time.Since(c.lastPong) > wsHeartbeatTimeout
	c.mu.Unlock()
	if stale {
		t.Fatal("a protocol-level Pong did not refresh lastPong — a client that " +
			"answers only control frames would be false-timed-out by the " +
			"heartbeat sweep (#5090)")
	}
}

// TestWS_RegisteredConnectionInstallsPongHandler drives the real registration
// path and asserts the hub credits an UNSOLICITED protocol Pong from the peer.
// It is the wiring test for the handler the unit test above exercises in
// isolation: without the SetPongHandler call at registration, this hangs up.
func TestWS_RegisteredConnectionInstallsPongHandler(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"protocol-pong-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // auth_challenge
	if err := conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: reg["registration_token"],
		CLIBackend:        "claude",
	}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}
	readMsg(t, conn) // auth_ok

	// An unsolicited Pong is legal (RFC 6455 §5.5.3) and is exactly what a
	// keepalive-answering client emits. The hub must accept it without closing.
	if err := conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write pong control frame: %v", err)
	}

	// The connection must still be usable afterwards: a hub that mishandled the
	// control frame would have torn it down instead of answering.
	if err := conn.WriteJSON(WSMessage{Type: "ping", Seq: 7}); err != nil {
		t.Fatalf("write ping after pong control frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := readMsg(t, conn)
	if got.Type != "pong" || got.Seq != 7 {
		t.Fatalf("after an unsolicited protocol Pong the hub answered %q seq=%d, want pong seq=7 "+
			"— the control frame disrupted the connection (#5090)", got.Type, got.Seq)
	}
}

// TestWS_WriteProtocolPingIsNilSafe guards the helper against the shutdown race
// its callers live in: heartbeatLoop can tick while a connection is being torn
// down. A panic there would take the hub process with it.
func TestWS_WriteProtocolPingIsNilSafe(t *testing.T) {
	if err := writeProtocolPing(nil); err != nil {
		t.Fatalf("writeProtocolPing(nil) = %v, want nil", err)
	}
}

// TestWS_ProtocolPingDeadlineIsShorterThanHeartbeatInterval pins the invariant
// that keeps a wedged peer from stacking up heartbeat goroutines: the bounded
// write must give up well before the next tick schedules another one.
func TestWS_ProtocolPingDeadlineIsShorterThanHeartbeatInterval(t *testing.T) {
	if wsProtocolPingDeadline >= wsHeartbeatInterval {
		t.Fatalf("wsProtocolPingDeadline (%v) must be shorter than wsHeartbeatInterval (%v), "+
			"or a peer that has stopped reading accumulates blocked heartbeat writes",
			wsProtocolPingDeadline, wsHeartbeatInterval)
	}
}

// TestWS_HeartbeatIntervalLeavesRoomForProxyIdleTimers records WHY the interval
// is what it is. The flap this fixes was an idle-timer reap; the keepalive is
// only worth sending if it comfortably outpaces the shortest idle timeout in
// the path (the OCI load balancer's 300s default, and nginx's 60s
// proxy_read_timeout default before this repo's Ingress raises it to 3600).
func TestWS_HeartbeatIntervalLeavesRoomForProxyIdleTimers(t *testing.T) {
	const shortestPlausibleProxyIdleTimeout = 60 * time.Second
	// Two ticks must fit inside the shortest idle window, so a single dropped
	// or delayed ping cannot by itself let the timer expire.
	if 2*wsHeartbeatInterval > shortestPlausibleProxyIdleTimeout {
		t.Fatalf("wsHeartbeatInterval (%v) is too long: two ticks (%v) exceed the shortest "+
			"plausible proxy idle timeout (%v), so one missed keepalive lets an "+
			"intermediary reap the socket frameless (#5090)",
			wsHeartbeatInterval, 2*wsHeartbeatInterval, shortestPlausibleProxyIdleTimeout)
	}
}
