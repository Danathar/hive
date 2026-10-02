package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"strings"
	"testing"
)

func TestFeedbackRedactsAndBuildsFallbackURL(t *testing.T) {
	req := feedbackReportRequest{
		Title:              "Bug with token ghp_secret123",
		Description:        "bearer abc.def user@example.com password=opensesame",
		RequestType:        feedbackTypeBug,
		TargetRepo:         feedbackTargetHive,
		IncludeDiagnostics: true,
		Diagnostics:        &feedbackDiagnostics{HiveID: "hive-one", Page: "/?token=secret", Agents: []feedbackAgentDiagnostic{{Name: "scanner", Repo: "private/repo"}}},
	}
	if err := validateFeedbackRequest(&req); err != nil {
		t.Fatal(err)
	}
	sanitizeFeedbackRequest(&req)
	body := buildFeedbackIssueBody(req)
	if strings.Contains(body, "ghp_secret") || strings.Contains(body, "abc.def") || strings.Contains(body, "user@example.com") || strings.Contains(body, "opensesame") || strings.Contains(body, "private/repo") {
		t.Fatalf("feedback body leaked sensitive data:\n%s", body)
	}
	if !strings.Contains(feedbackFallbackURL(req), "github.com/hivecommons/hive/issues/new") {
		t.Fatalf("fallback did not target hive repo")
	}
}

func TestFeedbackReportStandaloneReturnsFallback(t *testing.T) {
	s, _ := apiServer(t)
	body := `{"title":"A useful bug report","description":"Something went wrong in the dashboard","request_type":"bug","target_repo":"docs"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/report", strings.NewReader(body))
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out feedbackReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !strings.Contains(out.FallbackURL, "github.com/hivecommons/docs/issues/new") {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestFeedbackHubRelayCarriesHiveIDEvenWithoutDiagnostics(t *testing.T) {
	t.Setenv(spoke.EnvHeartbeatKey, "test-heartbeat")
	var got feedbackReportRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != feedbackHubIngestPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-heartbeat" {
			t.Fatalf("missing heartbeat bearer")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(feedbackReportResponse{OK: true, IssueNumber: 7, IssueURL: "https://github.com/hivecommons/hive/issues/7"})
	}))
	defer hub.Close()
	s := NewServer(0, dismissLogger())
	s.deps = &Dependencies{Config: &config.Config{HiveID: "hive-one", Hub: config.HubConfig{Enabled: true, URL: hub.URL, HiveType: "hosted"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/report", strings.NewReader(`{"title":"A useful bug report","description":"Something went wrong in the dashboard","request_type":"bug","include_diagnostics":false}`))
	markOwnerRequest(req)
	s.handleFeedbackReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got.HiveID != "hive-one" {
		t.Fatalf("relayed hive_id = %q", got.HiveID)
	}
	if got.Diagnostics != nil {
		t.Fatalf("diagnostics should remain excluded, got %+v", got.Diagnostics)
	}
}

func TestFeedbackStaticUIWiring(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{"data-action=\"openFeedbackModal\"", "installFeedbackCapture();", "FEEDBACK_DRAFT_KEY", "feedbackRedact", "/api/feedback/report"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	open := jsFunctionBody(t, html, "function openFeedbackModal()")
	if strings.Contains(open, "window.prompt") || strings.Contains(open, "alert(") || strings.Contains(open, "confirm(") {
		t.Fatal("feedback modal uses a native browser dialog")
	}
	submit := jsFunctionBody(t, html, "async function submitFeedbackReport()")
	if !strings.Contains(submit, "fetch('/api/feedback/report'") {
		t.Fatal("feedback submit does not post to the feedback endpoint")
	}
}
