package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestOpenAPIDeclaredTypeMatchesEmittedJSON is the #5574 guard.
//
// The two guards that already exist both leave this defect class open:
//
//   - TestOpenAPISpecCoversEveryRegisteredRoute (#4912) proves every route is
//     DOCUMENTED. It never reads a schema, so a route documented entirely in
//     placeholders passes it.
//   - TestOpenAPISchemaFieldsExistOnGoTypes (#5077) proves no spec property is
//     INVENTED, by walking the spec against the Go type the handler marshals.
//     It cannot see this one for two independent reasons: handleInferenceModels
//     builds an inline map[string]interface{} rather than a named struct, so
//     there is no type to pin it to; and its walk returns early on any object
//     with no `properties`, which is precisely the placeholder shape at fault.
//
// So `models` was declared `items: {"type": "object"}` while every value the
// handler can assign is a []string — fetchInferenceModelsForBackendDetailed,
// inferenceStaticModelAliases and intersectEntitled all return []string. A
// client generated from the spec decodes into a struct and dies with
//
//	json: cannot unmarshal object into Go value of type client.ModelOption
//
// which is the error the #5423 VHS fixture produced before its shape was
// corrected against the code rather than against the spec.
//
// This guard closes the direction those two miss, and does it the only way that
// cannot itself go stale: it runs the REAL handler and compares the JSON kind
// actually emitted against the kind the spec declares. Nothing here restates
// the spec by hand, so the test cannot agree with a wrong spec.
//
// It is deliberately narrow. It asserts the declared type of specific documented
// leaves, not that the whole payload is fully specified — dashboard/openapi.json
// currently carries 125 bare `{"type": "object"}` placeholders (16 under
// /api/status alone), and a guard demanding they all be filled in would fail
// permanently and be skipped, protecting nothing. Extending the table below is
// the cheap way to bring another leaf under the check.
func TestOpenAPIDeclaredTypeMatchesEmittedJSON(t *testing.T) {
	// Mock inference endpoint advertising two models, so the handler takes its
	// primary discovery path rather than the static-alias fallback.
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "openai/gpt-5"},
				{"id": "claude_opus_4.8"},
			},
		})
	}))
	defer mockSrv.Close()

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {mockSrv.URL}}

	req := httptest.NewRequest("GET", "/api/inference/models/vllm", nil)
	req.SetPathValue("backend", "vllm")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handleInferenceModels: code = %d, want 200", w.Code)
	}
	var emitted map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &emitted); err != nil {
		t.Fatalf("decoding handler response: %v", err)
	}

	models, ok := emitted["models"].([]any)
	if !ok {
		t.Fatalf("handler emitted models as %T, want a JSON array", emitted["models"])
	}
	if len(models) == 0 {
		t.Fatal("handler emitted an empty models array; this guard needs at least one " +
			"element to compare against the declared item type")
	}

	// The declared item type for /api/inference/models/{backend}.models[].
	declared := declaredItemType(t, "/api/inference/models/{backend}", "models")

	// Compare against every emitted element, so a mixed array cannot slip
	// through on the strength of its first entry.
	for i, m := range models {
		if got := jsonKindOf(m); got != declared {
			t.Errorf("dashboard/openapi.json declares "+
				"/api/inference/models/{backend}.models[].type = %q, but handleInferenceModels "+
				"emitted a %s at index %d (%#v).\n"+
				"Every source the handler can assign to `models` is a []string "+
				"(fetchInferenceModelsForBackendDetailed, inferenceStaticModelAliases, "+
				"intersectEntitled), so the SPEC is the wrong side here. A client generated "+
				"from it fails with: json: cannot unmarshal object into Go value of type "+
				"client.ModelOption",
				declared, got, i, m)
		}
	}
}

// declaredItemType reads the declared `type` of the items of an array-valued
// property in the 200 response of GET path, reading the spec from disk rather
// than from any in-test copy of it.
func declaredItemType(t *testing.T, path, property string) string {
	t.Helper()

	raw, err := os.ReadFile(openAPISpecPath)
	if err != nil {
		t.Fatalf("reading %s: %v", openAPISpecPath, err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", openAPISpecPath, err)
	}

	op, ok := doc.Paths[path]["get"]
	if !ok {
		t.Fatalf("%s documents no GET %s; this guard has gone stale", openAPISpecPath, path)
	}
	schema := op.Responses["200"].Content["application/json"].Schema
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s 200 schema has no properties", path)
	}
	prop, ok := props[property].(map[string]any)
	if !ok {
		t.Fatalf("GET %s 200 schema does not document %q", path, property)
	}
	if prop["type"] != "array" {
		t.Fatalf("GET %s .%s is declared type %v, want array", path, property, prop["type"])
	}
	items, ok := prop["items"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s .%s declares no items schema", path, property)
	}
	declared, ok := items["type"].(string)
	if !ok {
		t.Fatalf("GET %s .%s.items declares no type", path, property)
	}
	return declared
}

// jsonKindOf names the OpenAPI primitive type corresponding to a value decoded
// from JSON into any, so a declared `type` can be compared against what a
// handler really sent.
func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
