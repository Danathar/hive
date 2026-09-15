package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

const testHiveAppBotLogin = "hive[bot]"

type levelHoldServer struct {
	mu            sync.Mutex
	comments      []string
	commentAuthor string
	labelAuthor   string
	removes       int
}

func (s *levelHoldServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			author := s.commentAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			out := make([]map[string]any, 0, len(s.comments))
			for _, body := range s.comments {
				out = append(out, map[string]any{"body": body, "user": map[string]string{"login": author}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			body, _ := io.ReadAll(r.Body)
			var payload gh.IssueComment
			_ = json.Unmarshal(body, &payload)
			s.comments = append(s.comments, payload.GetBody())
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			author := s.labelAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": author},
				"label":      map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			s.removes++
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLevelHoldClient(t *testing.T, s *levelHoldServer) *Client {
	t.Helper()
	c := NewClientForTest(s.start(t).URL, "acme/widget", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	return c
}

func heldPR() *gh.PullRequest {
	return &gh.PullRequest{Number: gh.Ptr(11), Title: gh.Ptr("fix"), Body: gh.Ptr("safe change"), Labels: []*gh.Label{{Name: gh.Ptr("hold")}}}
}

func TestReleaseLevelHoldAfterPromotion(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}}
	c := newLevelHoldClient(t, s)
	c.prHoldLabel = func(agent string) bool { return false }

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPR())
	if err != nil || !released || reason != "level-hold-released" {
		t.Fatalf("releaseLevelHoldIfEligible = (%v,%q,%v), want release", released, reason, err)
	}
	if s.removes != 1 {
		t.Fatalf("removes=%d, want one", s.removes)
	}
}

func TestReleaseLevelHoldDoesNotReleaseUnknownOrForgedHold(t *testing.T) {
	for _, tc := range []struct {
		name          string
		comments      []string
		commentAuthor string
		wantReason    string
	}{
		{name: "unknown", wantReason: "hold"},
		{name: "forged", comments: []string{levelHoldNotice("quality")}, commentAuthor: "alice", wantReason: "hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &levelHoldServer{comments: tc.comments, commentAuthor: tc.commentAuthor}
			c := newLevelHoldClient(t, s)
			c.prHoldLabel = func(agent string) bool { return false }
			released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPR())
			if err != nil || released || reason != tc.wantReason || s.removes != 0 {
				t.Fatalf("release=(%v,%q,%v) removes=%d", released, reason, err, s.removes)
			}
		})
	}
}

func TestReleaseLevelHoldRespectsPolicyAndHumanRelabel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labelAuthor string
		policyHolds bool
		wantReason  string
	}{
		{name: "policy still holds", policyHolds: true, wantReason: "level-hold-still-required"},
		{name: "human relabeled", labelAuthor: "alice", wantReason: "level-hold-not-app-labeled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}, labelAuthor: tc.labelAuthor}
			c := newLevelHoldClient(t, s)
			c.prHoldLabel = func(agent string) bool { return tc.policyHolds }
			released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPR())
			if err != nil || released || reason != tc.wantReason || s.removes != 0 {
				t.Fatalf("release=(%v,%q,%v) removes=%d", released, reason, err, s.removes)
			}
		})
	}
}

func TestEnsureLevelHoldNoticePostsAgentAndDoesNotDuplicate(t *testing.T) {
	s := &levelHoldServer{}
	c := newLevelHoldClient(t, s)

	if err := c.ensureLevelHoldNotice(context.Background(), "acme/widget", 11, "quality"); err != nil {
		t.Fatalf("first ensureLevelHoldNotice: %v", err)
	}
	if err := c.ensureLevelHoldNotice(context.Background(), "acme/widget", 11, "quality"); err != nil {
		t.Fatalf("second ensureLevelHoldNotice: %v", err)
	}
	if len(s.comments) != 1 {
		t.Fatalf("posted %d comments, want one", len(s.comments))
	}
	for _, want := range []string{levelHoldNoticePrefix, `"agent":"quality"`, "quality", "ACMM L3"} {
		if !strings.Contains(s.comments[0], want) {
			t.Fatalf("level hold notice missing %q:\n%s", want, s.comments[0])
		}
	}
}
