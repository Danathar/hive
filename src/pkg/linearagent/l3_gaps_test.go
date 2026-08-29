package linearagent

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// updatingPoster is a recordingPoster that also implements SessionUpdater.
type updatingPoster struct {
	recordingPoster
	mu      sync.Mutex
	updates map[string][]ExternalURL
}

func (p *updatingPoster) UpdateSessionExternalURLs(_ context.Context, sessionID string, urls []ExternalURL) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.updates == nil {
		p.updates = map[string][]ExternalURL{}
	}
	p.updates[sessionID] = append(p.updates[sessionID], urls...)
	return nil
}

func TestTracker_ActiveSessionForIssue(t *testing.T) {
	tr := NewTracker()
	tr.Observe(createdEvent())
	if _, ok := tr.ActiveSessionForIssue("ENG-42"); ok {
		t.Fatal("acked-but-unkicked session must not hold the issue")
	}
	tr.SetAgent("sess-1", "quality")
	s, ok := tr.ActiveSessionForIssue("ENG-42")
	if !ok || s.Agent != "quality" || s.ID != "sess-1" {
		t.Fatalf("held = %+v, %v", s, ok)
	}
	tr.SetState("sess-1", SessionStateFinished, ActivityResponse)
	if _, ok := tr.ActiveSessionForIssue("ENG-42"); ok {
		t.Fatal("finished session must release the issue")
	}
}

func TestResponder_HandlePROpened(t *testing.T) {
	poster := &updatingPoster{}
	tracker := NewTracker()
	r := NewResponder(poster, func(string, string) error { return nil }, staticResolver("quality"), tracker, quietLogger())
	r.HandleSessionEvent(createdEvent())

	// Unrelated agent: nothing.
	r.HandlePROpened("scanner", "acme/widget", 7, "https://github.com/acme/widget/pull/7")
	if n := len(poster.snapshot()); n != 2 {
		t.Fatalf("activities after unrelated PR = %d, want 2", n)
	}

	r.HandlePROpened("quality", "acme/widget", 8, "https://github.com/acme/widget/pull/8")
	acts := poster.snapshot()
	if len(acts) != 3 || acts[2].Content.Type != ActivityAction || !strings.Contains(acts[2].Content.Parameter, "acme/widget#8") {
		t.Fatalf("activities = %+v", acts)
	}
	poster.mu.Lock()
	urls := poster.updates["sess-1"]
	poster.mu.Unlock()
	if len(urls) != 1 || urls[0].URL != "https://github.com/acme/widget/pull/8" || urls[0].Label != "acme/widget#8" {
		t.Fatalf("external urls = %+v", urls)
	}
}

func TestClient_UpdateSessionExternalURLs(t *testing.T) {
	api := newFakeLinearAPI(t)
	api.respond = func(w http.ResponseWriter, req fakeGraphQLRequest) {
		w.Write([]byte(`{"data":{"agentSessionUpdate":{"success":true}}}`))
	}
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})
	if err := c.UpdateSessionExternalURLs(context.Background(), "sess-1", []ExternalURL{{Label: "PR", URL: "https://x/pull/1"}}); err != nil {
		t.Fatalf("UpdateSessionExternalURLs: %v", err)
	}
	req := api.requests[0]
	if !strings.Contains(req.Query, "agentSessionUpdate") || req.Variables["id"] != "sess-1" {
		t.Errorf("request = %+v", req)
	}
	input := req.Variables["input"].(map[string]interface{})
	urls := input["externalUrls"].([]interface{})
	if len(urls) != 1 || urls[0].(map[string]interface{})["url"] != "https://x/pull/1" {
		t.Errorf("externalUrls = %v", urls)
	}
}
