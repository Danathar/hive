package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func swapAgyModelsProbe(t *testing.T, fn func() ([]string, error)) {
	t.Helper()
	prev := runAgyModelsProbe
	runAgyModelsProbe = fn
	t.Cleanup(func() { runAgyModelsProbe = prev })
}

func TestParseAgyModelsOutput(t *testing.T) {
	out := bytes.NewBufferString(strings.Join([]string{
		"Fetching available models...",
		"gemini-3.7-flash-high\tGemini 3.7 Flash (High)",
		"language-server chatter without a record",
		"claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)",
		"gemini-3.7-flash-high\tduplicate",
	}, "\n"))
	got, err := parseAgyModelsOutput(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"gemini-3.7-flash-high", "claude-sonnet-4-6"}
	if !equalStrings(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestParseAgyModelsOutputRejectsChangedFormat(t *testing.T) {
	_, err := parseAgyModelsOutput(bytes.NewBufferString("Fetching available models...\nno records here\n"))
	if err == nil {
		t.Fatal("output with no tab-delimited records must fail, not become an authoritative empty catalog")
	}
}

func TestQueryCLIModelsAgyLiveInventory(t *testing.T) {
	swapAgyModelsProbe(t, func() ([]string, error) {
		return []string{"gemini-3.7-flash-high", "claude-sonnet-4-6"}, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels(agyBackendID)
	if r.fallback {
		t.Fatalf("live agy inventory must be authoritative: %+v", r)
	}
	if !equalStrings(r.models, []string{"gemini-3.7-flash-high", "claude-sonnet-4-6"}) {
		t.Fatalf("models = %v, want probe order preserved", r.models)
	}
}

func TestQueryCLIModelsAgyFailureUsesStaticFallback(t *testing.T) {
	swapAgyModelsProbe(t, func() ([]string, error) {
		return nil, errors.New("signed out")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels(agyBackendID)
	if !r.fallback {
		t.Fatalf("failed agy probe must be marked fallback: %+v", r)
	}
	if !equalStrings(r.models, agyStaticModels) {
		t.Fatalf("models = %v, want static fallback %v", r.models, agyStaticModels)
	}
}

func TestExecAgyModelsProbeBinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := execAgyModelsProbe(); err == nil || !strings.Contains(err.Error(), "binary not on PATH") {
		t.Fatalf("missing agy binary error = %v", err)
	}
}

func TestDashboardOffersAgyWithDedicatedModels(t *testing.T) {
	src := backendPickerSource(t)
	if !pickerOffers(jsBackendList(t, src, "KNOWN_BACKENDS"), agyBackendID) {
		t.Fatal("dashboard method picker does not offer Google Antigravity (agy)")
	}
	if !pickerOffers(jsBackendList(t, src, "CLI_BACKENDS"), agyBackendID) {
		t.Fatal("dashboard drops agy model discovery because CLI_BACKENDS omits it")
	}
	for _, want := range []string{
		"agy: AGY_CLI_MODELS",
		"Google Antigravity (agy)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("dashboard agy surface missing %q", want)
		}
	}

	start := strings.Index(src, "const AGY_CLI_MODELS = [")
	if start < 0 {
		t.Fatal("AGY_CLI_MODELS fallback is missing")
	}
	rest := src[start:]
	end := strings.Index(rest, "];\n")
	if end < 0 {
		t.Fatal("AGY_CLI_MODELS fallback is unterminated")
	}
	matches := regexp.MustCompile(`value: '([^']+)'`).FindAllStringSubmatch(rest[:end], -1)
	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, match[1])
	}
	if !equalStrings(got, agyStaticModels) {
		t.Fatalf("frontend agy fallback = %v, Go fallback = %v; the first paint and API fallback must agree", got, agyStaticModels)
	}
}

func TestHandleBackendsServesAgyInventory(t *testing.T) {
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	// Keep this handler test offline: every CLI query is already cached, while
	// the agy entry carries the distinctive inventory asserted below.
	for _, backend := range []string{"claude", "copilot", "gemini", "goose", "bob"} {
		s.cliModels.set(backend, cliModelResult{models: []string{"placeholder"}, fallback: true})
	}
	wantModels := []string{"gemini-3.7-flash-high", "gpt-oss-120b-medium"}
	s.cliModels.set(agyBackendID, cliModelResult{models: wantModels, fallback: false})

	w := httptest.NewRecorder()
	s.handleBackends(w, httptest.NewRequest("GET", "/api/config/backends", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var backends []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Models   []string `json:"models"`
		Fallback bool     `json:"fallback"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &backends); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, backend := range backends {
		if backend.ID != agyBackendID {
			continue
		}
		if backend.Name != "Google Antigravity (agy)" {
			t.Errorf("agy name = %q", backend.Name)
		}
		if backend.Fallback {
			t.Error("cached live agy inventory was mislabeled as fallback")
		}
		if !equalStrings(backend.Models, wantModels) {
			t.Errorf("agy models = %v, want %v", backend.Models, wantModels)
		}
		return
	}
	t.Fatal("/api/config/backends omitted agy")
}
