package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests assert OBSERVABLE behaviour: that a real Close frame with code
// 1012 and the reason string actually arrives on a real socket. They do not
// assert that a function was called (kubestellar/hive#5388 — four "guard fails
// green" incidents in one week came from exactly that shortcut).
//
// WHAT THEY CANNOT COVER, stated plainly: none of this exercises a real pod
// roll. There is no kubelet here, no SIGTERM delivery, no
// terminationGracePeriodSeconds, and no rolling-update surge. The drain is
// invoked directly. That the hook is WIRED to SIGTERM in cmd/hive is covered
// separately and structurally in shutdown_hooks_test.go; that a real roll
// produces a 1012 at the relay can only be confirmed by observing a live
// upgrade on a hosted spoke.

// drainTestPeer is one end-to-end contributor socket: a real HTTP server that
// upgrades, the server-side *websocket.Conn the hub would hold, and the client
// side the "relay" reads close frames from.
type drainTestPeer struct {
	server   *httptest.Server
	clientWS *websocket.Conn
	serverWS *websocket.Conn
}

func (p *drainTestPeer) Close() {
	if p.clientWS != nil {
		_ = p.clientWS.Close()
	}
	if p.serverWS != nil {
		_ = p.serverWS.Close()
	}
	if p.server != nil {
		p.server.Close()
	}
}

// newDrainTestPeer stands up a genuine WebSocket pair. Nothing is mocked: the
// close frame under test travels over a real TCP connection.
func newDrainTestPeer(t *testing.T) *drainTestPeer {
	t.Helper()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	var (
		mu       sync.Mutex
		serverWS *websocket.Conn
		ready    = make(chan struct{})
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		serverWS = conn
		mu.Unlock()
		close(ready)
		// Hold the handler open; the hub owns this conn for the rest of the test.
		<-r.Context().Done()
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		_ = client.Close()
		srv.Close()
		t.Fatal("server side of the websocket never became ready")
	}

	mu.Lock()
	sws := serverWS
	mu.Unlock()

	peer := &drainTestPeer{server: srv, clientWS: client, serverWS: sws}
	t.Cleanup(peer.Close)
	return peer
}

// observedClose is what the client end actually saw.
type observedClose struct {
	code   int
	reason string
	err    error
}

// awaitClose reads on the client socket until the peer closes it, and reports
// the close code and reason the CLIENT observed — which is the only thing the
// relay ever gets to see.
func awaitClose(ws *websocket.Conn, within time.Duration) observedClose {
	_ = ws.SetReadDeadline(time.Now().Add(within))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				return observedClose{code: ce.Code, reason: ce.Text}
			}
			return observedClose{code: -1, err: err}
		}
	}
}

func drainTestHub(conns map[string]*ContributorConnection) *ContributeWSHub {
	return &ContributeWSHub{
		connections: conns,
		logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestDrainForShutdownWritesServiceRestartCloseFrame is the core assertion: a
// real relay on the other end of a real socket receives close code 1012 with
// the upgrade reason, not a bare 1006.
func TestDrainForShutdownWritesServiceRestartCloseFrame(t *testing.T) {
	peer := newDrainTestPeer(t)

	hub := drainTestHub(map[string]*ContributorConnection{
		"conn-1": {ws: peer.serverWS, profile: &ContributorProfile{GitHubUsername: "alice"}},
	})

	if n := hub.DrainForShutdown(); n != 1 {
		t.Fatalf("DrainForShutdown() = %d, want 1", n)
	}

	got := awaitClose(peer.clientWS, 5*time.Second)
	if got.err != nil {
		t.Fatalf("client did not observe a websocket close frame: %v", got.err)
	}
	if got.code != websocket.CloseServiceRestart {
		t.Errorf("close code = %d, want %d (CloseServiceRestart/1012)", got.code, websocket.CloseServiceRestart)
	}
	if got.code == websocket.CloseAbnormalClosure {
		t.Error("client saw 1006 — the frameless close this issue exists to eliminate")
	}
	if got.reason != wsDrainReason {
		t.Errorf("close reason = %q, want %q", got.reason, wsDrainReason)
	}
}

// TestDrainForShutdownClosesEveryConnection pins that the drain walks the WHOLE
// map — a drain that closed only the first socket would still pass a
// single-connection test.
func TestDrainForShutdownClosesEveryConnection(t *testing.T) {
	const peerCount = 3

	peers := make([]*drainTestPeer, 0, peerCount)
	conns := make(map[string]*ContributorConnection, peerCount)
	for i := 0; i < peerCount; i++ {
		p := newDrainTestPeer(t)
		peers = append(peers, p)
		conns[string(rune('a'+i))] = &ContributorConnection{
			ws:      p.serverWS,
			profile: &ContributorProfile{GitHubUsername: "user" + string(rune('a'+i))},
		}
	}

	hub := drainTestHub(conns)
	if n := hub.DrainForShutdown(); n != peerCount {
		t.Fatalf("DrainForShutdown() = %d, want %d", n, peerCount)
	}

	for i, p := range peers {
		got := awaitClose(p.clientWS, 5*time.Second)
		if got.err != nil {
			t.Errorf("peer %d observed no close frame: %v", i, got.err)
			continue
		}
		if got.code != websocket.CloseServiceRestart || got.reason != wsDrainReason {
			t.Errorf("peer %d: close = (%d, %q), want (%d, %q)",
				i, got.code, got.reason, websocket.CloseServiceRestart, wsDrainReason)
		}
	}
}

// TestDrainForShutdownIsBounded pins the design constraint that the drain
// cannot stall shutdown. It does NOT assert a precise duration — that would be
// a flaky wall-clock test — it asserts the budget is enforced as an upper
// bound with generous headroom for CI.
func TestDrainForShutdownIsBounded(t *testing.T) {
	// Sockets whose peer has vanished: the close-frame write can block against
	// wsCloseFrameDeadline, which is the stall this budget exists to bound.
	conns := make(map[string]*ContributorConnection, maxWSConnections)
	for i := 0; i < maxWSConnections; i++ {
		p := newDrainTestPeer(t)
		// Kill the client end without a handshake so the server-side write has no
		// reader.
		_ = p.clientWS.UnderlyingConn().Close()
		conns[string(rune('A'+i%26))+string(rune('a'+i/26))] = &ContributorConnection{ws: p.serverWS}
	}

	hub := drainTestHub(conns)

	start := time.Now()
	hub.DrainForShutdown()
	elapsed := time.Since(start)

	// The budget is 2s. Allow one in-flight close to overrun by a full
	// wsCloseFrameDeadline past the deadline check, plus CI slack.
	limit := wsDrainBudget + wsCloseFrameDeadline + 3*time.Second
	if elapsed > limit {
		t.Errorf("drain of %d dead sockets took %v, want <= %v — an unbounded drain "+
			"can outlive terminationGracePeriodSeconds and get SIGKILLed mid-shutdown",
			maxWSConnections, elapsed, limit)
	}
}

// TestDrainForShutdownDoesNotTearDownOtherInstancesConnections is the SURGE
// case (kubestellar/hive#5322).
//
// The deployment is maxSurge=1/maxUnavailable=0, so the replacement pod is
// Ready and serving BEFORE the old pod receives SIGTERM. A relay can therefore
// already have reconnected to the NEW hub when the OLD one drains. The old
// hub's drain must touch only the sockets in its own connection map: the new
// hub's connection is a different process, a different map, and a different TCP
// socket, and must survive intact.
//
// Modelled here as two hub values with disjoint maps, which is exactly the
// relationship two hub processes have.
func TestDrainForShutdownDoesNotTearDownOtherInstancesConnections(t *testing.T) {
	oldPeer := newDrainTestPeer(t)
	newPeer := newDrainTestPeer(t)

	oldHub := drainTestHub(map[string]*ContributorConnection{
		"conn-old": {ws: oldPeer.serverWS, profile: &ContributorProfile{GitHubUsername: "alice"}},
	})
	newHub := drainTestHub(map[string]*ContributorConnection{
		"conn-new": {ws: newPeer.serverWS, profile: &ContributorProfile{GitHubUsername: "alice"}},
	})

	// The OLD pod gets SIGTERM and drains.
	if n := oldHub.DrainForShutdown(); n != 1 {
		t.Fatalf("old hub drained %d connections, want 1", n)
	}

	// The old socket is closed with 1012 — expected and correct.
	if got := awaitClose(oldPeer.clientWS, 5*time.Second); got.code != websocket.CloseServiceRestart {
		t.Errorf("old socket close code = %d, want %d", got.code, websocket.CloseServiceRestart)
	}

	// The connection on the NEW hub must be untouched and still carrying traffic.
	if newHub.ActiveCount() != 1 {
		t.Errorf("new hub ActiveCount() = %d, want 1 — the old pod's drain "+
			"must not deregister the replacement's contributors", newHub.ActiveCount())
	}

	// Prove liveness rather than merely absence-of-close: write and read a real
	// message across the new socket AFTER the old hub drained.
	if err := newPeer.serverWS.WriteMessage(websocket.TextMessage, []byte("still-alive")); err != nil {
		t.Fatalf("new hub's socket was broken by the old pod's drain: %v", err)
	}
	_ = newPeer.clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := newPeer.clientWS.ReadMessage()
	if err != nil {
		t.Fatalf("new hub's relay could not read after the old pod drained: %v", err)
	}
	if string(payload) != "still-alive" {
		t.Errorf("payload = %q, want %q", payload, "still-alive")
	}
}

// TestDrainForShutdownHandlesEmptyAndNilSafely covers the shapes the shutdown
// path can legitimately hit: no contributors connected at all, an entry with no
// live socket (mirrors cleanupLoop's nil-guard), and a nil hub — the last being
// what a Server whose contribute routes were never registered hands back.
func TestDrainForShutdownHandlesEmptyAndNilSafely(t *testing.T) {
	if n := drainTestHub(map[string]*ContributorConnection{}).DrainForShutdown(); n != 0 {
		t.Errorf("empty hub drained %d, want 0", n)
	}

	wsLess := drainTestHub(map[string]*ContributorConnection{
		"no-socket": {profile: &ContributorProfile{GitHubUsername: "ghost"}},
		"nil-entry": nil,
	})
	if n := wsLess.DrainForShutdown(); n != 0 {
		t.Errorf("hub of socketless entries drained %d, want 0", n)
	}

	var nilHub *ContributeWSHub
	if n := nilHub.DrainForShutdown(); n != 0 {
		t.Errorf("nil hub drained %d, want 0", n)
	}

	var nilSrv *Server
	if n := nilSrv.DrainContributorsForShutdown(); n != 0 {
		t.Errorf("nil server drained %d, want 0", n)
	}

	srvNoHub := &Server{}
	if n := srvNoHub.DrainContributorsForShutdown(); n != 0 {
		t.Errorf("server without a contribute hub drained %d, want 0", n)
	}
}

// TestDrainForShutdownDoesNotHoldHubLockDuringClose pins the locking shape the
// implementation inherited from cleanupLoop: sockets are snapshotted under
// h.mu and closed OUTSIDE it. Holding the hub-wide lock across a close frame
// that can block for wsCloseFrameDeadline per wedged peer would stall every
// other hub operation for up to maxWSConnections seconds.
//
// Asserted behaviourally: while the drain is running against a dead peer,
// another goroutine must still be able to acquire the hub lock.
func TestDrainForShutdownDoesNotHoldHubLockDuringClose(t *testing.T) {
	conns := make(map[string]*ContributorConnection, 4)
	for i := 0; i < 4; i++ {
		p := newDrainTestPeer(t)
		_ = p.clientWS.UnderlyingConn().Close()
		conns[string(rune('a'+i))] = &ContributorConnection{ws: p.serverWS}
	}
	hub := drainTestHub(conns)

	lockAcquired := make(chan struct{})
	go func() {
		// Give the drain a moment to be mid-close, then contend for the lock.
		time.Sleep(50 * time.Millisecond)
		hub.mu.Lock()
		hub.mu.Unlock() //nolint:staticcheck // acquiring the lock is the assertion
		close(lockAcquired)
	}()

	done := make(chan struct{})
	go func() {
		hub.DrainForShutdown()
		close(done)
	}()

	select {
	case <-lockAcquired:
	case <-time.After(wsDrainBudget + 5*time.Second):
		t.Fatal("could not acquire h.mu while a drain was in progress — the drain " +
			"is closing sockets under the hub lock")
	}
	<-done
}
