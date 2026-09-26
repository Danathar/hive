package jev

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedUsage struct {
	agent, model string
	in, out      int64
}

type fakeUsage struct {
	mu   sync.Mutex
	recs []recordedUsage
}

func (f *fakeUsage) Record(agent, model string, in, out int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, recordedUsage{agent, model, in, out})
}

type auditRec struct {
	actor, action, agent string
	fields               map[string]any
}

type fakeAudit struct {
	mu   sync.Mutex
	recs []auditRec
}

func (f *fakeAudit) Record(actor, action, agent string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, auditRec{actor, action, agent, fields})
}

func newTestServer(t *testing.T, fp *fakeProvider, identity string, enabled map[string]bool, key string) (*Server, *fakeUsage, *fakeAudit) {
	usage := &fakeUsage{}
	audit := &fakeAudit{}
	srv := &Server{
		Identify: func(*http.Request) string { return identity },
		Enabled:  func(a string) bool { return enabled[a] },
		Key:      func() string { return key },
		Timeout:  2 * time.Second,
		Client:   newFakeClient(t, fp),
		Usage:    usage,
		Audit:    audit,
	}
	return srv, usage, audit
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, DecidePath, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const goodBody = `{"type":"choice","question":"dup?","options":["yes","no"]}`

// TestServer_RefusesUnidentifiedAndDisabled: no identity → 403; identified but
// jev_mode off → 403. Neither resolves the key nor touches the provider — that
// is the "default off: no network calls" acceptance.
func TestServer_RefusesUnidentifiedAndDisabled(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	keyCalls := 0
	for _, tc := range []struct {
		name, identity string
		enabled        map[string]bool
		wantMsg        string
	}{
		{"unidentified", "", map[string]bool{"scanner": true}, "could not be identified"},
		{"disabled", "scanner", map[string]bool{"scanner": false}, "jev_mode is off for agent scanner"},
		{"unknown agent", "ghost", map[string]bool{"scanner": true}, "jev_mode is off for agent ghost"},
	} {
		srv, usage, audit := newTestServer(t, fp, tc.identity, tc.enabled, "k")
		srv.Key = func() string { keyCalls++; return "k" }
		rec := post(srv.Handler(), goodBody)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), tc.wantMsg) {
			t.Errorf("%s: got %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if len(usage.recs) != 0 || len(audit.recs) != 0 {
			t.Errorf("%s: refused call must not be budgeted or audited", tc.name)
		}
	}
	if fp.calls != 0 || keyCalls != 0 {
		t.Errorf("refused calls reached the key (%d) or provider (%d)", keyCalls, fp.calls)
	}
}

func TestServer_NotReadyWithoutKey(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	srv, _, _ := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "  ")
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "JEV_API_KEY") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatal("no key must mean no provider call")
	}
}

func TestServer_BadRequests(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	srv, _, _ := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k")
	for name, body := range map[string]string{
		"not json":   `{`,
		"one option": `{"type":"choice","question":"q","options":["a"]}`,
		"no type":    `{"question":"q"}`,
	} {
		rec := post(srv.Handler(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, DecidePath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d", rec.Code)
	}
	if fp.calls != 0 {
		t.Fatal("invalid requests must not reach the provider")
	}
}

// TestServer_SuccessBudgetsAndAudits: a good call is answered, its input tokens
// land in the usage sink under the agent and model, and the audit entry carries
// type/confidence/tokens but never the question or state.
func TestServer_SuccessBudgetsAndAudits(t *testing.T) {
	fp := &fakeProvider{answer: `{"model":"typesafe/jev-test","answers":{"decision":{"type":"choice","choice":"yes","confidence":0.77,"probabilities":{"yes":0.8,"no":0.2}}},"usage":{"input_tokens":150}}`}
	srv, usage, audit := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k-secret")
	rec := post(srv.Handler(), `{"type":"choice","question":"SECRET-QUESTION","options":["yes","no"],"state":{"body":"SECRET-STATE"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var res Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Answer != "yes" || res.Confidence != 0.77 || res.InputTokens != 150 {
		t.Errorf("result = %+v", res)
	}
	if fp.lastAuth != "Bearer k-secret" {
		t.Errorf("provider auth = %q", fp.lastAuth)
	}
	if len(usage.recs) != 1 || usage.recs[0] != (recordedUsage{"scanner", "typesafe/jev-test", 150, 0}) {
		t.Errorf("usage = %+v", usage.recs)
	}
	if len(audit.recs) != 1 {
		t.Fatalf("audit = %+v", audit.recs)
	}
	a := audit.recs[0]
	if a.action != "jev_decision" || a.agent != "scanner" || a.fields["outcome"] != "success" || a.fields["question_type"] != "choice" || a.fields["input_tokens"] != 150 {
		t.Errorf("audit entry = %+v", a)
	}
	for _, v := range a.fields {
		if s, ok := v.(string); ok && strings.Contains(s, "SECRET") {
			t.Errorf("audit must not carry question/state content: %v", a.fields)
		}
	}
}

func TestServer_ProviderFailureAuditedNotBudgeted(t *testing.T) {
	fp := &fakeProvider{status: http.StatusBadGateway, answer: `upstream down`}
	srv, usage, audit := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k")
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "HTTP 502") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(usage.recs) != 0 {
		t.Error("a failed call has no tokens to budget")
	}
	if len(audit.recs) != 1 || audit.recs[0].fields["outcome"] != "failure" {
		t.Errorf("audit = %+v", audit.recs)
	}
}
