package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func outreachValidationServer(t *testing.T, filename, patch string, additions int, created *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ahead",
				"files": []map[string]any{{
					"filename":  filename,
					"status":    "modified",
					"additions": additions,
					"patch":     patch,
				}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			if created != nil {
				*created++
			}
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func outreachValidationClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestValidateOutreachPRRequestRequiresClaimEvidence(t *testing.T) {
	srv := outreachValidationServer(t, "docs/guide.md", "@@ -0,0 +1 @@\n+Verified feature", 1, nil)
	defer srv.Close()
	c := outreachValidationClient(t, srv)

	reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "outreach/guide", Agent: "outreach", Body: "## Summary\n\nA guide.",
	})
	if err != nil {
		t.Fatalf("validateOutreachPRRequest: %v", err)
	}
	if !strings.Contains(reason, "must include a Claim evidence section") {
		t.Fatalf("reason = %q, want missing-evidence rejection", reason)
	}
}

func TestValidateOutreachPRRequestRejectsAddedRegulatoryTerms(t *testing.T) {
	terms := []string{"HIPAA", "PCI DSS", "GDPR", "SOC 2", "FedRAMP", "FIPS-140"}
	for _, term := range terms {
		t.Run(term, func(t *testing.T) {
			patch := "@@ -0,0 +1 @@\n+" + term + " ready"
			srv := outreachValidationServer(t, "docs/guide.md", patch, 1, nil)
			defer srv.Close()
			c := outreachValidationClient(t, srv)

			reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{
				Repo: "o/r", Base: "main", Head: "outreach/guide", Agent: "outreach",
				Body: "## Claim evidence\n\n- Feature: src/feature.go",
			})
			if err != nil {
				t.Fatalf("validateOutreachPRRequest: %v", err)
			}
			if !strings.Contains(reason, "adds regulatory term") || !strings.Contains(reason, "docs/guide.md") {
				t.Fatalf("reason = %q, want regulatory-term rejection", reason)
			}
		})
	}
}

func TestValidateOutreachPRRequestRejectsRegulatoryPRMetadata(t *testing.T) {
	srv := outreachValidationServer(t, "docs/guide.md", "@@ -0,0 +1 @@\n+Healthcare session guide", 1, nil)
	defer srv.Close()
	c := outreachValidationClient(t, srv)

	reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "outreach/guide", Agent: "outreach",
		Title: "docs: add HIPAA session guide",
		Body:  "## Claim evidence\n\n- Session behavior: src/session.go",
	})
	if err != nil {
		t.Fatalf("validateOutreachPRRequest: %v", err)
	}
	if !strings.Contains(reason, "PR metadata contains regulatory term") {
		t.Fatalf("reason = %q, want regulatory-metadata rejection", reason)
	}
}

func TestValidateOutreachPRRequestAllowsGroundedNonRegulatoryContent(t *testing.T) {
	srv := outreachValidationServer(t, "docs/guide.md", "@@ -0,0 +1 @@\n+Feature X is implemented by src/feature.go", 1, nil)
	defer srv.Close()
	c := outreachValidationClient(t, srv)

	reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "outreach/guide", Agent: " Outreach ",
		Body: "## Claim Evidence\n\n- Feature X: src/feature.go",
	})
	if err != nil || reason != "" {
		t.Fatalf("validation = reason %q, error %v; want allowed", reason, err)
	}
}

func TestValidateOutreachPRRequestIgnoresOtherAgents(t *testing.T) {
	c := &Client{}
	reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{Agent: "scanner"})
	if err != nil || reason != "" {
		t.Fatalf("non-outreach validation = reason %q, error %v; want bypass", reason, err)
	}
}

func TestValidateOutreachPRRequestFailsClosedWhenAddedPatchUnavailable(t *testing.T) {
	srv := outreachValidationServer(t, "docs/large-guide.md", "", 12, nil)
	defer srv.Close()
	c := outreachValidationClient(t, srv)

	reason, err := c.validateOutreachPRRequest(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "outreach/guide", Agent: "outreach",
		Body: "## Claim evidence\n\n- Feature X: src/feature.go",
	})
	if err != nil {
		t.Fatalf("validateOutreachPRRequest: %v", err)
	}
	if !strings.Contains(reason, "cannot inspect added text in docs/large-guide.md") {
		t.Fatalf("reason = %q, want unavailable-patch rejection", reason)
	}
}

func TestPRRequestWatcherRejectsOutreachRegulatoryClaimBeforeCreate(t *testing.T) {
	created := 0
	srv := outreachValidationServer(t, "docs/guide.md", "@@ -0,0 +1 @@\n+HIPAA compliant sessions", 1, &created)
	defer srv.Close()
	c := outreachValidationClient(t, srv)
	c.prAuthz = func(string, int) error { return nil }

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Base: "main", Head: "outreach/guide", Title: "docs: add healthcare guide", Agent: "outreach",
		Body: "## Claim evidence\n\n- Session behavior: src/session.go",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("regulatory claim created %d PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("rejected request was not quarantined: %v", err)
	}
	result, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "regulatory term") {
		t.Fatalf("result lacks actionable rejection: %s", result)
	}
}
