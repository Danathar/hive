package jev

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRun_RefusedWhenModeOff: with HIVE_JEV_MODE unset (the default) the CLI
// exits 1 with a pointer at jev_mode and never contacts an endpoint.
func TestRun_RefusedWhenModeOff(t *testing.T) {
	t.Setenv(ModeEnvVar, "")
	called := false
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ep.Close()
	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--question", "q", "--option", "a", "--option", "b", "--endpoint", ep.URL}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "jev_mode: assist") || called {
		t.Fatalf("code=%d called=%v stderr=%s", code, called, errb.String())
	}
}

func TestRun_UsageErrors(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	for name, args := range map[string][]string{
		"no args":         nil,
		"unknown sub":     {"frob"},
		"missing q":       {"decide", "--option", "a", "--option", "b"},
		"two state flags": {"decide", "--question", "q", "--option", "a", "--option", "b", "--state", "{}", "--state-stdin"},
		"one option":      {"decide", "--question", "q", "--option", "a", "--endpoint", "http://127.0.0.1:1"},
		"positional":      {"decide", "--question", "q", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errb); code != 2 {
			t.Errorf("%s: code=%d stderr=%s", name, code, errb.String())
		}
	}
}

// TestRun_DecideRoundTrip drives the real CLI against a fake decision endpoint:
// the request carries the parsed flags (option descriptions, levels, stdin
// state, advisory identity header), and the endpoint's JSON is printed as-is.
func TestRun_DecideRoundTrip(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	t.Setenv(agentEnvVar, "scanner")
	var got Request
	var gotAuth string
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DecidePath || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Proxy-Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Result{Answer: "1.5", Confidence: 0.6, Model: "m", InputTokens: 9})
	}))
	defer ep.Close()
	t.Setenv(EndpointEnvVar, ep.URL)

	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--type", "score", "--question", "risk?", "--level", "low", "--level", "high", "--state-stdin"},
		strings.NewReader(`{"diff":"x"}`), &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
	if got.Type != TypeScore || len(got.Levels) != 2 || string(got.State) != `{"diff":"x"}` {
		t.Errorf("request = %+v", got)
	}
	if gotAuth != "hive scanner" {
		t.Errorf("Proxy-Authorization = %q", gotAuth)
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Answer != "1.5" || res.InputTokens != 9 {
		t.Errorf("stdout = %s (%v)", out.String(), err)
	}

	// name=description options become Descriptions.
	out.Reset()
	code = Run([]string{"decide", "--question", "which?", "--option", "a=first thing", "--option", "b"}, strings.NewReader(""), &out, &errb)
	if code != 0 || got.Options[0] != "a" || got.Descriptions["a"] != "first thing" || len(got.Descriptions) != 1 {
		t.Errorf("code=%d request=%+v", code, got)
	}
}

// TestRun_EndpointRefusalIsExit1: a 403 from the hive (jev_mode off server-side)
// surfaces the server's reason and exits 1, not 2.
func TestRun_EndpointRefusalIsExit1(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, errorBody{"jev_mode is off for agent scanner"})
	}))
	defer ep.Close()
	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--question", "q", "--option", "a", "--option", "b", "--endpoint", ep.URL}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "jev_mode is off for agent scanner") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out.String(), errb.String())
	}
}
