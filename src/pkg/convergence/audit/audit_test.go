package audit

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadScopeReadsComponents(t *testing.T) {
	dir := t.TempDir()
	data := `{"components":[{"name":"hub","files":["b.go","a.go"],"content":"body","findings":[{"title":"Finding","files":["z.go"],"labels":["ci"]}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "components.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	scope, err := LoadScope(dir)
	if err != nil {
		t.Fatalf("LoadScope returned error: %v", err)
	}
	if len(scope.Components) != 1 {
		t.Fatalf("components = %d, want 1", len(scope.Components))
	}
	component := scope.Components[0]
	if component.Name != "hub" || component.Content != "body" {
		t.Fatalf("component = %+v, want hub/body", component)
	}
	if !reflect.DeepEqual(component.Files, []string{"b.go", "a.go"}) {
		t.Fatalf("files = %#v, want source order", component.Files)
	}
	if got := component.Findings[0].Labels; !reflect.DeepEqual(got, []string{"ci"}) {
		t.Fatalf("labels = %#v, want ci", got)
	}
}

func TestLoadScopeRejectsEmptyScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "components.json"), []byte(`{"components":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadScope(dir); err == nil {
		t.Fatal("LoadScope returned nil error for empty scope")
	}
}

func TestDuplicateKeyNormalizesTitleAndFiles(t *testing.T) {
	got := duplicateKey(Finding{Title: "  Same   Finding ", Files: []string{"b.go", "a.go"}})
	want := "same finding|a.go\x00b.go"
	if got != want {
		t.Fatalf("duplicateKey = %q, want %q", got, want)
	}
}

func TestDuplicateKeyPrefersSemanticIdentity(t *testing.T) {
	got := duplicateKey(Finding{
		Title:         "ignored",
		Files:         []string{"ignored.go"},
		SubjectDigest: "sha256:abc",
		Predicate:     "review.finding",
		Location:      "pkg/a.go:10",
	})
	if got == "" || got == "ignored|ignored.go" {
		t.Fatalf("duplicateKey semantic identity = %q, want non-empty semantic key", got)
	}
}
