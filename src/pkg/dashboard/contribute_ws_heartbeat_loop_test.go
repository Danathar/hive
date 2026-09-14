package dashboard

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func heartbeatTestHub(c *ContributorConnection) *ContributeWSHub {
	return &ContributeWSHub{
		connections: map[string]*ContributorConnection{"conn": c},
		logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
}

func TestHeartbeatLoopWithTicksClosesTimedOutConnection(t *testing.T) {
	peer := newDrainTestPeer(t)
	c := &ContributorConnection{
		ws:       peer.serverWS,
		profile:  &ContributorProfile{GitHubUsername: "alice"},
		lastPong: time.Now().Add(-time.Minute),
	}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()

	heartbeatTestHub(c).heartbeatLoopWithTicks(c, ticks, time.Millisecond)

	got := awaitClose(peer.clientWS, 2*time.Second)
	if got.err != nil {
		t.Fatalf("client did not observe heartbeat timeout close: %v", got.err)
	}
	if got.code != websocket.CloseGoingAway || got.reason != "heartbeat timeout: no pong within the heartbeat window" {
		t.Fatalf("close = (%d, %q), want (%d, heartbeat timeout reason)", got.code, got.reason, websocket.CloseGoingAway)
	}
}

func TestHeartbeatLoopWithTicksClosesOnJSONPingWriteFailure(t *testing.T) {
	peer := newDrainTestPeer(t)
	// A connection may be deregistered just after the heartbeat registration
	// check. Closing the hub end models that shutdown race and makes the next
	// JSON write fail synchronously, without depending on TCP peer teardown.
	if err := peer.serverWS.Close(); err != nil {
		t.Fatalf("close server websocket: %v", err)
	}
	c := &ContributorConnection{
		ws:       peer.serverWS,
		profile:  &ContributorProfile{GitHubUsername: "alice"},
		lastPong: time.Now(),
	}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()

	heartbeatTestHub(c).heartbeatLoopWithTicks(c, ticks, time.Hour)
}

func TestHeartbeatLoopWithTicksSendsJSONPingAndStopsWithTickStream(t *testing.T) {
	peer := newDrainTestPeer(t)
	c := &ContributorConnection{
		ws:       peer.serverWS,
		profile:  &ContributorProfile{GitHubUsername: "alice"},
		lastPong: time.Now(),
	}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	close(ticks)

	heartbeatTestHub(c).heartbeatLoopWithTicks(c, ticks, time.Hour)

	_ = peer.clientWS.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := peer.clientWS.ReadMessage()
	if err != nil {
		t.Fatalf("read heartbeat JSON ping: %v", err)
	}
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal heartbeat message: %v", err)
	}
	if msg.Type != "ping" {
		t.Fatalf("heartbeat message type = %q, want ping", msg.Type)
	}
}

func TestHeartbeatLoopWithTicksStopsForDeregisteredConnection(t *testing.T) {
	c := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "alice"}}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()

	(&ContributeWSHub{connections: make(map[string]*ContributorConnection)}).heartbeatLoopWithTicks(c, ticks, time.Hour)
}
