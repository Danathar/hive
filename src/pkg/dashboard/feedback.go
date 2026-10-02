package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

const (
	feedbackMaxRequestBytes     = 15 << 20
	feedbackMaxTextBytes        = 64 << 10
	feedbackMaxScreenshots      = 5
	feedbackMaxScreenshotBytes  = 2 << 20
	feedbackHubIngestPath       = "/api/feedback/ingest"
	feedbackForwardTimeout      = 20 * time.Second
	feedbackMaxHubResponseBytes = 8 << 10
	feedbackTargetHive          = "hive"
	feedbackTargetDocs          = "docs"
	feedbackTypeBug             = "bug"
	feedbackTypeFeature         = "feature"
	feedbackFallbackBaseHive    = "https://github.com/hivecommons/hive/issues/new"
	feedbackFallbackBaseDocs    = "https://github.com/hivecommons/docs/issues/new"
)

type feedbackConsoleError struct {
	Timestamp string `json:"timestamp,omitempty"`
	Level     string `json:"level,omitempty"`
	Message   string `json:"message,omitempty"`
	Source    string `json:"source,omitempty"`
}

type feedbackFailedAPICall struct {
	Timestamp string `json:"timestamp,omitempty"`
	Status    string `json:"status,omitempty"`
	Path      string `json:"path,omitempty"`
}

type feedbackAgentDiagnostic struct {
	Name    string `json:"name,omitempty"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	State   string `json:"state,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Org     string `json:"org,omitempty"`
}

type feedbackDiagnostics struct {
	Version             string                    `json:"version,omitempty"`
	Commit              string                    `json:"commit,omitempty"`
	Channel             string                    `json:"channel,omitempty"`
	ACMMLevel           string                    `json:"acmm_level,omitempty"`
	HiveID              string                    `json:"hive_id,omitempty"`
	Hosted              bool                      `json:"hosted,omitempty"`
	HubLinked           bool                      `json:"hub_linked,omitempty"`
	AgentCount          int                       `json:"agent_count,omitempty"`
	Agents              []feedbackAgentDiagnostic `json:"agents,omitempty"`
	IncludeProjectRepos bool                      `json:"include_project_repos,omitempty"`
	BrowserUA           string                    `json:"browser_user_agent,omitempty"`
	BrowserPlatform     string                    `json:"browser_platform,omitempty"`
	BrowserLanguage     string                    `json:"browser_language,omitempty"`
	ScreenSize          string                    `json:"screen_size,omitempty"`
	WindowSize          string                    `json:"window_size,omitempty"`
	Page                string                    `json:"page,omitempty"`
}

type feedbackReportRequest struct {
	Title              string                  `json:"title"`
	Description        string                  `json:"description"`
	RequestType        string                  `json:"request_type"`
	TargetRepo         string                  `json:"target_repo"`
	HiveID             string                  `json:"hive_id,omitempty"`
	Screenshots        []string                `json:"screenshots,omitempty"`
	IncludeDiagnostics bool                    `json:"include_diagnostics"`
	Diagnostics        *feedbackDiagnostics    `json:"diagnostics,omitempty"`
	ConsoleErrors      []feedbackConsoleError  `json:"console_errors,omitempty"`
	FailedAPICalls     []feedbackFailedAPICall `json:"failed_api_calls,omitempty"`
}

type feedbackReportResponse struct {
	OK          bool   `json:"ok"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	FallbackURL string `json:"fallback_url,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

type feedbackIssueResult struct {
	Number int
	URL    string
	ID     int64
}

var (
	feedbackTokenPattern  = regexp.MustCompile(`(?i)(ghp_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+`)
	feedbackBearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
	feedbackEmailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	feedbackKVPattern     = regexp.MustCompile(`(?i)(secret|token|password|key)\s*[:=]\s*[^\s,;]+`)
)

func (s *Server) handleFeedbackReport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, feedbackMaxRequestBytes+1))
	if err != nil {
		jsonError(w, "could not read request", http.StatusBadRequest)
		return
	}
	if len(body) > feedbackMaxRequestBytes {
		jsonError(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req feedbackReportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := validateFeedbackRequest(&req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sanitizeFeedbackRequest(&req)

	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.NPSHubLinked() {
		req.HiveID = strings.TrimSpace(s.deps.Config.HiveID)
		resp, status, err := s.forwardFeedbackToHub(r.Context(), req)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("feedback: hub relay failed", "status", status, "error", err)
			}
			jsonError(w, "could not deliver feedback - please try again later", http.StatusBadGateway)
			return
		}
		s.auditFromRequest(r, "feedback_submit", auditDetail("target", req.TargetRepo, "type", req.RequestType, "via", "hub", "issue", fmt.Sprintf("%d", resp.IssueNumber)), "")
		jsonResponse(w, resp)
		return
	}

	if token := s.feedbackUserToken(r); token != "" {
		result, warning, err := createFeedbackGitHubIssue(r.Context(), http.DefaultClient, token, req, feedbackGitHubAPIBase())
		if err == nil {
			s.auditFromRequest(r, "feedback_submit", auditDetail("target", req.TargetRepo, "type", req.RequestType, "via", "user", "issue", fmt.Sprintf("%d", result.Number)), "")
			jsonResponse(w, feedbackReportResponse{OK: true, IssueNumber: result.Number, IssueURL: result.URL, Warning: warning})
			return
		}
		if s.logger != nil {
			s.logger.Warn("feedback: user-token issue create failed; returning fallback", "error", err)
		}
	}
	fallback := feedbackFallbackURL(req)
	s.auditFromRequest(r, "feedback_submit", auditDetail("target", req.TargetRepo, "type", req.RequestType, "via", "fallback"), "")
	jsonResponse(w, feedbackReportResponse{OK: true, FallbackURL: fallback, Warning: "Open the prefilled GitHub issue and paste screenshots manually."})
}

func validateFeedbackRequest(req *feedbackReportRequest) error {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	if req.Title == "" || utf8.RuneCountInString(req.Title) > 200 {
		return errors.New("title is required and must be 200 characters or fewer")
	}
	if req.Description == "" {
		return errors.New("description is required")
	}
	if req.RequestType != feedbackTypeBug && req.RequestType != feedbackTypeFeature {
		return errors.New("request_type must be bug or feature")
	}
	if req.TargetRepo == "" {
		req.TargetRepo = feedbackTargetHive
	}
	if req.TargetRepo != feedbackTargetHive && req.TargetRepo != feedbackTargetDocs {
		return errors.New("target_repo must be hive or docs")
	}
	if len(req.Screenshots) > feedbackMaxScreenshots {
		return fmt.Errorf("at most %d screenshots are allowed", feedbackMaxScreenshots)
	}
	textBytes := len(req.Title) + len(req.Description) + len(mustFeedbackJSON(req.Diagnostics)) + len(mustFeedbackJSON(req.ConsoleErrors)) + len(mustFeedbackJSON(req.FailedAPICalls))
	if textBytes > feedbackMaxTextBytes {
		return errors.New("feedback text and diagnostics are too large")
	}
	for _, ss := range req.Screenshots {
		if _, err := decodeFeedbackDataURI(ss); err != nil {
			return err
		}
	}
	return nil
}

func mustFeedbackJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func sanitizeFeedbackRequest(req *feedbackReportRequest) {
	req.Title = truncateRunes(feedbackRedact(req.Title), 200)
	req.Description = truncateRunes(feedbackRedact(req.Description), 5000)
	if len(req.ConsoleErrors) > 20 {
		req.ConsoleErrors = req.ConsoleErrors[len(req.ConsoleErrors)-20:]
	}
	for i := range req.ConsoleErrors {
		req.ConsoleErrors[i].Message = truncateRunes(feedbackRedact(req.ConsoleErrors[i].Message), 500)
		req.ConsoleErrors[i].Source = truncateRunes(feedbackRedact(stripQuery(req.ConsoleErrors[i].Source)), 200)
	}
	if len(req.FailedAPICalls) > 20 {
		req.FailedAPICalls = req.FailedAPICalls[len(req.FailedAPICalls)-20:]
	}
	for i := range req.FailedAPICalls {
		req.FailedAPICalls[i].Path = truncateRunes(feedbackRedact(stripQuery(req.FailedAPICalls[i].Path)), 200)
		req.FailedAPICalls[i].Status = truncateRunes(feedbackRedact(req.FailedAPICalls[i].Status), 40)
	}
	if !req.IncludeDiagnostics {
		req.Diagnostics = nil
		return
	}
	if req.Diagnostics != nil {
		sanitizeFeedbackDiagnostics(req.Diagnostics)
	}
}

func sanitizeFeedbackDiagnostics(d *feedbackDiagnostics) {
	d.Version = truncateRunes(feedbackRedact(d.Version), 80)
	d.Commit = truncateRunes(feedbackRedact(d.Commit), 80)
	d.Channel = truncateRunes(feedbackRedact(d.Channel), 40)
	d.ACMMLevel = truncateRunes(feedbackRedact(d.ACMMLevel), 40)
	d.HiveID = truncateRunes(feedbackRedact(d.HiveID), 120)
	d.BrowserUA = truncateRunes(feedbackRedact(d.BrowserUA), 300)
	d.BrowserPlatform = truncateRunes(feedbackRedact(d.BrowserPlatform), 80)
	d.BrowserLanguage = truncateRunes(feedbackRedact(d.BrowserLanguage), 40)
	d.ScreenSize = truncateRunes(feedbackRedact(d.ScreenSize), 40)
	d.WindowSize = truncateRunes(feedbackRedact(d.WindowSize), 40)
	d.Page = truncateRunes(feedbackRedact(stripQuery(d.Page)), 200)
	if len(d.Agents) > 50 {
		d.Agents = d.Agents[:50]
	}
	for i := range d.Agents {
		d.Agents[i].Name = truncateRunes(feedbackRedact(d.Agents[i].Name), 80)
		d.Agents[i].Backend = truncateRunes(feedbackRedact(d.Agents[i].Backend), 80)
		d.Agents[i].Model = truncateRunes(feedbackRedact(d.Agents[i].Model), 80)
		d.Agents[i].State = truncateRunes(feedbackRedact(d.Agents[i].State), 80)
		if !d.IncludeProjectRepos {
			d.Agents[i].Repo = ""
			d.Agents[i].Org = ""
		}
	}
}

func feedbackRedact(s string) string {
	s = feedbackTokenPattern.ReplaceAllString(s, "[REDACTED_TOKEN]")
	s = feedbackBearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = feedbackEmailPattern.ReplaceAllString(s, "[REDACTED_EMAIL]")
	s = feedbackKVPattern.ReplaceAllString(s, "$1=[REDACTED]")
	return s
}
func stripQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

func (s *Server) forwardFeedbackToHub(ctx context.Context, req feedbackReportRequest) (feedbackReportResponse, int, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return feedbackReportResponse{}, 0, errors.New("no hub credential configured")
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackForwardTimeout)
	defer cancel()
	endpoint := strings.TrimRight(strings.TrimSpace(s.deps.Config.Hub.URL), "/") + feedbackHubIngestPath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := npsNoRedirectClient().Do(hreq)
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	defer closeHTTPBody(resp.Body)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, feedbackMaxHubResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackReportResponse{}, resp.StatusCode, fmt.Errorf("hub answered %d", resp.StatusCode)
	}
	var out feedbackReportResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return feedbackReportResponse{}, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func (s *Server) feedbackUserToken(r *http.Request) string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	if s.hubProxied() {
		return ""
	}
	if s.directRouteAuthzEnabled() && s.sessionFromRequest(r) == nil {
		return ""
	}
	raw, err := os.ReadFile(userTokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func feedbackGitHubAPIBase() string { return "https://api.github.com" }

func feedbackRepo(req feedbackReportRequest) (owner, repo string) {
	if req.TargetRepo == feedbackTargetDocs {
		return "hivecommons", "docs"
	}
	return "hivecommons", "hive"
}
func feedbackLabels(req feedbackReportRequest) []string {
	if req.RequestType == feedbackTypeBug {
		return []string{"kind/bug", "user-feedback"}
	}
	return []string{"enhancement", "user-feedback"}
}

func createFeedbackGitHubIssue(ctx context.Context, client *http.Client, token string, req feedbackReportRequest, apiBase string) (feedbackIssueResult, string, error) {
	owner, repo := feedbackRepo(req)
	body := buildFeedbackIssueBody(req)
	labels := feedbackLabels(req)
	res, status, err := postGitHubIssue(ctx, client, apiBase, token, owner, repo, req.Title, body, labels)
	warning := ""
	if err != nil && status == http.StatusForbidden && len(labels) > 0 {
		res, _, err = postGitHubIssue(ctx, client, apiBase, token, owner, repo, req.Title, body, nil)
		warning = "Created without labels because GitHub denied label access."
	}
	if err != nil {
		return feedbackIssueResult{}, "", err
	}
	valid := make([]string, 0, len(req.Screenshots))
	for _, ss := range req.Screenshots {
		if _, err := decodeFeedbackDataURI(ss); err == nil {
			valid = append(valid, ss)
		}
	}
	if len(valid) > 0 {
		go uploadFeedbackScreenshots(context.Background(), client, apiBase, token, owner, repo, res.Number, valid)
	}
	return res, warning, nil
}

func buildFeedbackIssueBody(req feedbackReportRequest) string {
	var b strings.Builder
	b.WriteString(req.Description)
	b.WriteString("\n\n---\nSubmitted from the Hive spoke dashboard feedback form.\n")
	if req.TargetRepo == feedbackTargetDocs {
		b.WriteString("Target: Documentation\n")
	} else {
		b.WriteString("Target: Hive\n")
	}
	if req.Diagnostics != nil {
		b.WriteString("\n<details>\n<summary>Diagnostics</summary>\n\n")
		writeFeedbackDiagnostics(&b, req.Diagnostics)
		b.WriteString("\n</details>\n")
	}
	if len(req.ConsoleErrors) > 0 {
		b.WriteString(fmt.Sprintf("\n<details>\n<summary>Browser Console Errors (%d captured)</summary>\n\n", len(req.ConsoleErrors)))
		for _, e := range req.ConsoleErrors {
			b.WriteString(fmt.Sprintf("- `[%s]` **%s**: %s\n", e.Timestamp, e.Level, e.Message))
		}
		b.WriteString("\n</details>\n")
	}
	if len(req.FailedAPICalls) > 0 {
		b.WriteString(fmt.Sprintf("\n<details>\n<summary>Failed API Calls (%d captured)</summary>\n\n", len(req.FailedAPICalls)))
		for _, c := range req.FailedAPICalls {
			b.WriteString(fmt.Sprintf("- `[%s]` %s %s\n", c.Timestamp, c.Status, c.Path))
		}
		b.WriteString("\n</details>\n")
	}
	if len(req.Screenshots) > 0 {
		b.WriteString(fmt.Sprintf("\nScreenshots: %d attached; the dashboard will upload them as issue comments.\n", len(req.Screenshots)))
	}
	return truncateRunes(b.String(), 60000)
}

func writeFeedbackDiagnostics(b *strings.Builder, d *feedbackDiagnostics) {
	b.WriteString("| Field | Value |\n|---|---|\n")
	rows := [][2]string{{"Version", d.Version}, {"Commit", d.Commit}, {"Channel", d.Channel}, {"ACMM Level", d.ACMMLevel}, {"Hive ID", d.HiveID}, {"Hosted", fmt.Sprintf("%t", d.Hosted)}, {"Hub linked", fmt.Sprintf("%t", d.HubLinked)}, {"Agent count", fmt.Sprintf("%d", d.AgentCount)}, {"Browser UA", d.BrowserUA}, {"Browser platform", d.BrowserPlatform}, {"Browser language", d.BrowserLanguage}, {"Screen", d.ScreenSize}, {"Window", d.WindowSize}, {"Page", d.Page}}
	for _, r := range rows {
		if r[1] != "" {
			b.WriteString(fmt.Sprintf("| %s | %s |\n", r[0], strings.ReplaceAll(r[1], "|", "\\|")))
		}
	}
	if len(d.Agents) > 0 {
		b.WriteString("\nAgents:\n")
		for _, a := range d.Agents {
			b.WriteString(fmt.Sprintf("- %s: backend=%s model=%s state=%s\n", a.Name, a.Backend, a.Model, a.State))
		}
	}
}

func feedbackFallbackURL(req feedbackReportRequest) string {
	base := feedbackFallbackBaseHive
	if req.TargetRepo == feedbackTargetDocs {
		base = feedbackFallbackBaseDocs
	}
	q := url.Values{}
	q.Set("title", req.Title)
	q.Set("body", buildFeedbackIssueBody(req)+"\n\nScreenshots cannot be attached automatically on this path. Please paste them into this issue.")
	q.Set("labels", strings.Join(feedbackLabels(req), ","))
	return base + "?" + q.Encode()
}

func postGitHubIssue(ctx context.Context, client *http.Client, apiBase, token, owner, repo, title, body string, labels []string) (feedbackIssueResult, int, error) {
	payload := map[string]any{"title": title, "body": body}
	if labels != nil {
		payload["labels"] = labels
	}
	data, _ := json.Marshal(payload)
	u := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/issues"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return feedbackIssueResult{}, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return feedbackIssueResult{}, 0, err
	}
	defer closeHTTPBody(resp.Body)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackIssueResult{}, resp.StatusCode, fmt.Errorf("github create issue: %d", resp.StatusCode)
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		ID      int64  `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return feedbackIssueResult{}, resp.StatusCode, err
	}
	return feedbackIssueResult{Number: out.Number, URL: out.HTMLURL, ID: out.ID}, resp.StatusCode, nil
}

func decodeFeedbackDataURI(dataURI string) ([]byte, error) {
	parts := strings.SplitN(dataURI, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:image/") {
		return nil, errors.New("screenshots must be image data URIs")
	}
	b, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid screenshot data")
	}
	if len(b) > feedbackMaxScreenshotBytes {
		return nil, fmt.Errorf("each screenshot must be %d MiB or smaller", feedbackMaxScreenshotBytes>>20)
	}
	return b, nil
}

func uploadFeedbackScreenshots(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, issue int, screenshots []string) {
	_, _ = postGitHubComment(ctx, client, apiBase, token, owner, repo, issue, "Processing feedback screenshots…")
	var lines []string
	for i, ss := range screenshots {
		content, err := decodeFeedbackDataURI(ss)
		if err != nil {
			continue
		}
		ext := "png"
		if strings.HasPrefix(ss, "data:image/jpeg") {
			ext = "jpg"
		}
		path := fmt.Sprintf(".github/feedback-screenshots/%d/screenshot-%d.%s", issue, i+1, ext)
		dl, err := putGitHubContent(ctx, client, apiBase, token, owner, repo, path, content)
		if err == nil && dl != "" {
			lines = append(lines, fmt.Sprintf("![screenshot %d](%s)", i+1, dl))
		}
	}
	if len(lines) > 0 {
		_, _ = postGitHubComment(ctx, client, apiBase, token, owner, repo, issue, "Feedback screenshots:\n\n"+strings.Join(lines, "\n\n"))
	}
}

func putGitHubContent(ctx context.Context, client *http.Client, apiBase, token, owner, repo, path string, content []byte) (string, error) {
	payload := map[string]string{"message": "Add feedback screenshot", "content": base64.StdEncoding.EncodeToString(content)}
	data, _ := json.Marshal(payload)
	u := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/contents/" + url.PathEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer closeHTTPBody(resp.Body)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github upload: %d", resp.StatusCode)
	}
	var out struct {
		Content struct {
			DownloadURL string `json:"download_url"`
		} `json:"content"`
	}
	_ = json.Unmarshal(b, &out)
	return out.Content.DownloadURL, nil
}
func postGitHubComment(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, issue int, body string) (string, error) {
	payload := map[string]string{"body": body}
	data, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", strings.TrimRight(apiBase, "/"), owner, repo, issue)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github comment: %d", resp.StatusCode)
	}
	return "", nil
}
