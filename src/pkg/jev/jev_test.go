package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeProvider is a TypeSafe systemone stand-in. It records the last request
// body and answers with whatever the test set.
type fakeProvider struct {
	t        *testing.T
	last     map[string]any
	lastAuth string
	status   int
	answer   string
	calls    int
}

func (f *fakeProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.lastAuth = r.Header.Get("Authorization")
		f.last = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&f.last); err != nil {
			f.t.Fatalf("provider: bad JSON body: %v", err)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = w.Write([]byte(f.answer))
	})
}

func newFakeClient(t *testing.T, fp *fakeProvider) *Client {
	fp.t = t
	srv := httptest.NewServer(fp.handler())
	t.Cleanup(srv.Close)
	return NewClient(func() config.JevConfig { return config.JevConfig{Endpoint: srv.URL, Model: "typesafe/jev-test"} }, srv.Client())
}

func question(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	qs, ok := body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("request has no questions map: %v", body)
	}
	q, ok := qs[questionKey].(map[string]any)
	if !ok {
		t.Fatalf("request has no %q question: %v", questionKey, body)
	}
	return q
}

// TestDecide_ChoiceContract pins the Choice primitive: type "choice", criteria
// as an option→description map (null when undescribed), answer read from
// `choice`, confidence and probabilities passed through, bearer auth set.
func TestDecide_ChoiceContract(t *testing.T) {
	fp := &fakeProvider{answer: `{"model":"typesafe/jev-test","answers":{"decision":{"type":"choice","choice":"no","confidence":0.82,"probabilities":{"yes":0.09,"no":0.91}}},"usage":{"input_tokens":211}}`}
	c := newFakeClient(t, fp)
	res, err := c.Decide(context.Background(), Request{
		Type: TypeChoice, Question: "Does the diff touch secrets?",
		Options: []string{"yes", "no"}, Descriptions: map[string]string{"yes": "credential or CI permission change"},
		State: json.RawMessage(`{"files":["a.go"]}`),
	}, "k-123")
	if err != nil {
		t.Fatal(err)
	}
	if fp.lastAuth != "Bearer k-123" {
		t.Errorf("auth = %q", fp.lastAuth)
	}
	if fp.last["model"] != "typesafe/jev-test" {
		t.Errorf("model = %v", fp.last["model"])
	}
	q := question(t, fp.last)
	if q["type"] != "choice" || q["instructions"] != "Does the diff touch secrets?" {
		t.Errorf("question = %v", q)
	}
	crit, _ := q["criteria"].(map[string]any)
	if crit["yes"] != "credential or CI permission change" || crit["no"] != nil {
		t.Errorf("criteria = %v", q["criteria"])
	}
	if _, ok := crit["no"]; !ok {
		t.Errorf("undescribed option must still be a criteria key: %v", crit)
	}
	if res.Answer != "no" || res.Confidence != 0.82 || res.Probabilities["no"] != 0.91 || res.InputTokens != 211 {
		t.Errorf("result = %+v", res)
	}
}

// TestDecide_ChoiceOutsideOptionsRejected: a provider answer that is not one of
// the offered options is an error, never handed to the agent as an answer.
func TestDecide_ChoiceOutsideOptionsRejected(t *testing.T) {
	fp := &fakeProvider{answer: `{"answers":{"decision":{"type":"choice","choice":"maybe","confidence":0.9}}}`}
	c := newFakeClient(t, fp)
	_, err := c.Decide(context.Background(), Request{Type: TypeChoice, Question: "q", Options: []string{"yes", "no"}}, "k")
	if err == nil || !strings.Contains(err.Error(), `"maybe"`) {
		t.Fatalf("want out-of-set error, got %v", err)
	}
}

// TestDecide_ScoreContract pins the Score primitive: type "score", criteria as
// the ORDERED level array, answer from `score`, legend passed through, and a
// score outside 0..N-1 rejected.
func TestDecide_ScoreContract(t *testing.T) {
	fp := &fakeProvider{answer: `{"answers":{"decision":{"type":"score","score":1.43,"confidence":0.35,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"0":0,"1":0.57,"2":0.43}}},"usage":{"input_tokens":332}}`}
	c := newFakeClient(t, fp)
	res, err := c.Decide(context.Background(), Request{Type: TypeScore, Question: "How risky?", Levels: []string{"low", "mid", "high"}}, "k")
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, fp.last)
	if q["type"] != "score" {
		t.Errorf("type = %v", q["type"])
	}
	levels, _ := q["criteria"].([]any)
	if len(levels) != 3 || levels[0] != "low" || levels[2] != "high" {
		t.Errorf("criteria must be the ordered level array, got %v", q["criteria"])
	}
	if res.Answer != "1.43" || res.Confidence != 0.35 || res.Legend["1"] != "mid" || res.Probabilities["2"] != 0.43 {
		t.Errorf("result = %+v", res)
	}

	fp.answer = `{"answers":{"decision":{"type":"score","score":7}}}`
	if _, err := c.Decide(context.Background(), Request{Type: TypeScore, Question: "q", Levels: []string{"a", "b"}}, "k"); err == nil {
		t.Fatal("score outside the offered levels must be rejected")
	}
}

// TestDecide_ProbabilityIsNoul pins the mapping the issue's `probability` type
// has onto TypeSafe's Noul: wire type "noul", answer from `noul`, and — since
// the provider reports no confidence for a Noul — confidence derived as
// |2p−1| with a {yes,no} distribution.
func TestDecide_ProbabilityIsNoul(t *testing.T) {
	fp := &fakeProvider{answer: `{"answers":{"decision":{"type":"noul","noul":0.9}},"usage":{"input_tokens":360}}`}
	c := newFakeClient(t, fp)
	for _, typ := range []string{TypeProbability, "noul", "Probability"} {
		res, err := c.Decide(context.Background(), Request{Type: typ, Question: "Is this a duplicate?", Criteria: json.RawMessage(`{"true":"same root cause","false":"different"}`)}, "k")
		if err != nil {
			t.Fatalf("type %q: %v", typ, err)
		}
		q := question(t, fp.last)
		if q["type"] != "noul" {
			t.Errorf("type %q: wire type = %v, want noul", typ, q["type"])
		}
		crit, _ := q["criteria"].(map[string]any)
		if crit["true"] != "same root cause" {
			t.Errorf("criteria not forwarded verbatim: %v", q["criteria"])
		}
		if res.Answer != "0.9" || res.Probabilities["yes"] != 0.9 {
			t.Errorf("result = %+v", res)
		}
		if got := res.Confidence; got < 0.7999 || got > 0.8001 {
			t.Errorf("confidence = %v, want |2·0.9−1| = 0.8", got)
		}
	}
	// No criteria → none sent (the provider treats it as optional).
	_, _ = c.Decide(context.Background(), Request{Type: TypeProbability, Question: "q"}, "k")
	if _, has := question(t, fp.last)["criteria"]; has {
		t.Error("probability without criteria must omit the criteria field")
	}
	fp.answer = `{"answers":{"decision":{"type":"noul","noul":1.7}}}`
	if _, err := c.Decide(context.Background(), Request{Type: TypeProbability, Question: "q"}, "k"); err == nil {
		t.Fatal("noul outside 0..1 must be rejected")
	}
}

func TestDecide_ProviderErrors(t *testing.T) {
	fp := &fakeProvider{status: http.StatusUnauthorized, answer: `{"error":"bad key"}`}
	c := newFakeClient(t, fp)
	_, err := c.Decide(context.Background(), Request{Type: TypeProbability, Question: "q"}, "k")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("want HTTP 401 error carrying the body, got %v", err)
	}
	fp.status, fp.answer = 0, `{"answers":{}}`
	if _, err := c.Decide(context.Background(), Request{Type: TypeProbability, Question: "q"}, "k"); err == nil {
		t.Fatal("missing answer must be an error")
	}
	if _, err := c.Decide(context.Background(), Request{Type: TypeProbability, Question: "q"}, ""); err == nil || fp.calls != 2 {
		t.Fatalf("empty key must fail before any provider call (calls=%d, err=%v)", fp.calls, err)
	}
}

func TestRequestValidate(t *testing.T) {
	long := strings.Repeat("x", MaxQuestionBytes+1)
	cases := map[string]Request{
		"no question":             {Type: TypeChoice, Options: []string{"a", "b"}},
		"question too long":       {Type: TypeChoice, Question: long, Options: []string{"a", "b"}},
		"unknown type":            {Type: "essay", Question: "q"},
		"choice one option":       {Type: TypeChoice, Question: "q", Options: []string{"a"}},
		"choice duplicate":        {Type: TypeChoice, Question: "q", Options: []string{"a", "a"}},
		"choice empty option":     {Type: TypeChoice, Question: "q", Options: []string{"a", " "}},
		"choice with levels":      {Type: TypeChoice, Question: "q", Options: []string{"a", "b"}, Levels: []string{"x", "y"}},
		"choice stray desc":       {Type: TypeChoice, Question: "q", Options: []string{"a", "b"}, Descriptions: map[string]string{"c": "d"}},
		"score one level":         {Type: TypeScore, Question: "q", Levels: []string{"a"}},
		"score eleven levels":     {Type: TypeScore, Question: "q", Levels: strings.Split("a b c d e f g h i j k", " ")},
		"score with options":      {Type: TypeScore, Question: "q", Levels: []string{"a", "b"}, Options: []string{"x", "y"}},
		"probability with levels": {Type: TypeProbability, Question: "q", Levels: []string{"a", "b"}},
		"invalid state":           {Type: TypeProbability, Question: "q", State: json.RawMessage(`{`)},
		"state too big":           {Type: TypeProbability, Question: "q", State: json.RawMessage(`"` + strings.Repeat("s", MaxStateBytes) + `"`)},
		"invalid criteria":        {Type: TypeProbability, Question: "q", Criteria: json.RawMessage(`nope`)},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	ok := Request{Type: " CHOICE ", Question: "  q  ", Options: []string{" a ", "b"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Type != TypeChoice || ok.Question != "q" || ok.Options[0] != "a" {
		t.Errorf("normalization: %+v", ok)
	}
	score := Request{Type: TypeScore, Question: "q", Levels: []string{"a", "b"}}
	if err := score.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillRelDir(t *testing.T) {
	for backend, want := range map[string]string{
		"claude": ".claude/skills/jev-decide", "codex": "skills/jev-decide",
		"copilot": ".copilot/skills/jev-decide", "gemini": ".gemini/skills/jev-decide",
		"goose": ".config/goose/skills/jev-decide", "aider": "", "bob": "",
	} {
		if got := SkillRelDir(backend); got != want {
			t.Errorf("%s: %q, want %q", backend, got, want)
		}
	}
	if !strings.Contains(string(SkillMarkdown()), "hive jev decide") || !strings.HasPrefix(string(SkillMarkdown()), "---\nname: jev-decide") {
		t.Error("embedded skill must be the jev-decide SKILL.md with front matter")
	}
}
