package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_close_reason_test.go pins the fix for kubestellar/hive#5090.
//
// THE DEFECT: every hub-side close on this path was a bare conn.Close(), which
// shuts the TCP socket without sending a WebSocket Close frame. The client sees
// code 1006 (abnormal closure) with an empty reason — byte-for-byte identical
// to a yanked network cable. A contributor whose socket flapped every 30-90
// seconds therefore could not tell a deliberate server hangup from a network
// fault, and neither could anyone reading the relay log.
//
// Measured against a live hub before the fix: an unauthenticated probe received
//
//	30.1s msg:   {"type":"auth_failed","reason":"Authentication timeout"}
//	30.1s CLOSE  code= 1006 reason= ""
//
// The hub knew why it was hanging up, said so in a JSON message, and then closed
// in a way that carried none of it — and the heartbeat and stale-sweep closes do
// not send even that JSON.
//
// These tests read the CLOSE FRAME rather than the JSON message, because the
// frame is the part that was missing and the only part a client that has stopped
// reading application messages can still observe.

// readUntilClose drains application messages until the peer closes, then returns
// the close error. A nil return means the connection ended without one, which is
// itself the pre-fix behavior these tests exist to reject.
func readUntilClose(t *testing.T, conn *websocket.Conn) *websocket.CloseError {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr
			}
			t.Fatalf("connection ended without a close frame: %v", err)
		}
	}
}

// TestWS_AuthFailureClosesWithAStatedReason drives a real refusal — an invalid
// registration token — and asserts the client can learn WHY from the close frame
// alone. Before the fix this arrived as 1006 with no text.
func TestWS_AuthFailureClosesWithAStatedReason(t *testing.T) {
	_, ts := setupWSTest(t)
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // auth_challenge

	if err := conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: "not-a-real-token",
	}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}

	closeErr := readUntilClose(t, conn)
	if closeErr.Code == websocket.CloseAbnormalClosure {
		t.Fatal("the hub closed without a close frame (1006) — a deliberate refusal is " +
			"indistinguishable from a network drop (#5090)")
	}
	if closeErr.Code != websocket.ClosePolicyViolation {
		t.Errorf("close code = %d, want %d (policy violation)", closeErr.Code, websocket.ClosePolicyViolation)
	}
	if closeErr.Text == "" {
		t.Error("the close frame carries no reason, so the client still cannot tell why it was hung up on")
	}
}

// TestWS_MissingTokenClosesWithAStatedReason covers the other pre-auth refusal,
// which reaches a different call site. The two must not drift apart: a client
// that omits the token entirely deserves the same explanation as one that sends
// a wrong one.
func TestWS_MissingTokenClosesWithAStatedReason(t *testing.T) {
	_, ts := setupWSTest(t)
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // auth_challenge

	if err := conn.WriteJSON(WSMessage{Type: "auth_response"}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}

	closeErr := readUntilClose(t, conn)
	if closeErr.Code != websocket.ClosePolicyViolation || closeErr.Text == "" {
		t.Errorf("missing-token refusal closed with code=%d text=%q; want a policy violation with a stated reason",
			closeErr.Code, closeErr.Text)
	}
}

// TestCleanupSweepDoesNotCloseUnderTheHubLock pins the lock discipline the close
// frame made load-bearing.
//
// closeWithReason writes a Close frame with a deadline, so it can block for up to
// wsCloseFrameDeadline against a peer that has stopped reading — precisely what a
// stale connection is. Holding h.mu across that would freeze the hub-wide lock
// for up to a second per stale socket, serially; against the maxWSConnections cap
// of 50 that is a worst case near a minute with no contributor able to register.
// The bare c.ws.Close() this replaced could not block, so the risk arrived WITH
// the close frame.
//
// A source-level assertion because the failure is a lock-scope property, not an
// observable output: a runtime test would have to wedge a real peer's TCP
// receive window to make the write block, which is far more machinery than the
// invariant is worth.
func TestCleanupSweepDoesNotCloseUnderTheHubLock(t *testing.T) {
	src := fileSource(t, "src/pkg/dashboard/contribute_ws.go")

	start := strings.Index(src, "func (h *ContributeWSHub) cleanupLoop()")
	if start < 0 {
		t.Fatal("cleanupLoop was not found")
	}
	body := src[start : start+strings.Index(src[start:], "\nfunc ")]

	unlockIdx := strings.LastIndex(body, "h.mu.Unlock()")
	closeIdx := strings.Index(body, "closeWithReason(")
	if unlockIdx < 0 || closeIdx < 0 {
		t.Fatal("cleanupLoop no longer unlocks or no longer closes; re-check this invariant")
	}
	if closeIdx < unlockIdx {
		t.Error("cleanupLoop calls closeWithReason while holding h.mu — a blocking " +
			"control-frame write can stall the whole hub for up to a second per stale socket")
	}
	// The sockets have to be collected first, or moving the close out would mean
	// closing connections still registered in the map.
	if !strings.Contains(body, "staleConns") {
		t.Error("stale sockets must be collected and deregistered under the lock, then closed outside it")
	}
}
