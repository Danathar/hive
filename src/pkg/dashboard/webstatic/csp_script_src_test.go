package webstatic

// In-package coverage for the pure CSP hash machinery (#3848/#3907, ADR-0016).
// The end-to-end assertions — that every document pkg/dashboard actually
// serves satisfies the policy served with it — live in
// pkg/dashboard/csp_script_src_test.go next to the handlers; this file pins
// the extraction/hashing/rewrite primitives themselves.

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// cspDirective returns the named directive from a CSP header value, or "".
// Mirrors the helper of the same name in pkg/dashboard's tests.
func cspDirective(csp, name string) string {
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if part == name || strings.HasPrefix(part, name+" ") {
			return part
		}
	}
	return ""
}

func TestExtractInlineScripts(t *testing.T) {
	doc := []byte(`<!DOCTYPE html><html><head>
<script>first()</script>
<script src="/app.js"></script>
<SCRIPT type="application/json">{"data":"block"}</SCRIPT>
<script>
  multi();
  line();
</script>
</head><body></body></html>`)

	scripts := ExtractInlineScripts(doc)
	if len(scripts) != 3 {
		t.Fatalf("extracted %d scripts, want 3 (src= scripts excluded, data blocks and uppercase tags included)", len(scripts))
	}
	// Document order is part of the contract: hashes are matched against the
	// exact element the browser tokenizes at that position.
	if got := string(scripts[0]); got != "first()" {
		t.Errorf("scripts[0] = %q, want %q", got, "first()")
	}
	if got := string(scripts[1]); got != `{"data":"block"}` {
		t.Errorf("scripts[1] = %q, want the JSON data block — non-executable blocks are hashed too", got)
	}
	if got := string(scripts[2]); !strings.Contains(got, "multi();") || !strings.Contains(got, "line();") {
		t.Errorf("scripts[2] = %q, want the full multi-line body", got)
	}
	for i, s := range scripts {
		if strings.Contains(string(s), "app.js") {
			t.Errorf("scripts[%d] captured a src= script — external scripts are covered by URL sources, not hashes", i)
		}
	}

	if got := ExtractInlineScripts([]byte(`<html><p>no scripts</p><script src="x.js"></script></html>`)); len(got) != 0 {
		t.Errorf("document with only external scripts yielded %d inline scripts, want 0", len(got))
	}
}

func TestCSPScriptHash(t *testing.T) {
	// Pinned against an INDEPENDENTLY computed value (sha256 of the exact
	// bytes, standard base64 with padding, per WHATWG CSP §8.2): if the digest,
	// encoding, or token quoting ever drifts, every served policy goes stale at
	// once and the dashboard renders blank in CSP3 browsers.
	const want = "'sha256-bhHHL3z2vDgxUt0W3dWQOrprscmda2Y5pLsLg4GF+pI='"
	if got := CSPScriptHash([]byte("alert(1)")); got != want {
		t.Fatalf("CSPScriptHash(alert(1)) = %s, want %s", got, want)
	}
	// The digest is over the element's exact text: any byte change changes it.
	if CSPScriptHash([]byte("alert(1)")) == CSPScriptHash([]byte("alert(1) ")) {
		t.Fatal("trailing-byte change did not change the hash token")
	}
	tokenRe := regexp.MustCompile(`^'sha256-[A-Za-z0-9+/]+=*'$`)
	if got := CSPScriptHash([]byte("")); !tokenRe.MatchString(got) {
		t.Fatalf("empty-content hash %q is not a well-formed sha256 source token", got)
	}
}

func TestScriptSrcElemSources(t *testing.T) {
	doc := []byte(`<html>
<script>alert(1)</script>
<script>alert(1)</script>
<script>other()</script>
</html>`)
	sources := ScriptSrcElemSources(doc)

	parts := strings.Fields(sources)
	if len(parts) == 0 || parts[0] != "'self'" {
		t.Fatalf("source list must start with 'self', got %q", sources)
	}
	// Two DISTINCT scripts → exactly two hash tokens: duplicates dedupe.
	if len(parts) != 3 {
		t.Fatalf("got %d source tokens (%q), want 3: 'self' + one hash per distinct script", len(parts), sources)
	}
	for _, script := range [][]byte{[]byte("alert(1)"), []byte("other()")} {
		if h := CSPScriptHash(script); !strings.Contains(sources, h) {
			t.Errorf("source list %q missing hash %s for script %q", sources, h, script)
		}
	}

	if got := ScriptSrcElemSources([]byte("<html>no scripts</html>")); got != "'self'" {
		t.Errorf("script-free document: sources = %q, want \"'self'\"", got)
	}
}

// TestApplyDocumentScriptSrcElem pins the header-rewrite helper the dynamic
// handlers depend on. (Moved in-package from pkg/dashboard with the #5565
// extraction; the handler-integration lanes stay in pkg/dashboard's tests.)
func TestApplyDocumentScriptSrcElem(t *testing.T) {
	doc := []byte(`<html><head><script>alert("ours")</script></head></html>`)
	wantHash := CSPScriptHash([]byte(`alert("ours")`))

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; script-src-elem 'self' 'sha256-stale='; script-src-attr 'unsafe-inline'; style-src 'self'")
	ApplyDocumentScriptSrcElem(rec, doc)
	csp := rec.Header().Get("Content-Security-Policy")

	elem := cspDirective(csp, "script-src-elem")
	if !strings.Contains(elem, wantHash) {
		t.Errorf("rewritten script-src-elem missing the document's hash: %q", elem)
	}
	if strings.Contains(elem, "sha256-stale=") {
		t.Errorf("rewritten script-src-elem kept a stale hash: %q", elem)
	}
	// Neighbouring directives must be untouched.
	for _, want := range []string{"default-src 'self'", "script-src 'self' 'unsafe-inline'", "script-src-attr 'unsafe-inline'", "style-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("rewrite disturbed a neighbouring directive: %q missing from %q", want, csp)
		}
	}

	// A script-free document yields a hash-free (but still closed) directive.
	rec2 := httptest.NewRecorder()
	rec2.Header().Set("Content-Security-Policy", "script-src-elem 'self' 'sha256-stale='")
	ApplyDocumentScriptSrcElem(rec2, []byte("<html>no scripts</html>"))
	if got := cspDirective(rec2.Header().Get("Content-Security-Policy"), "script-src-elem"); got != "script-src-elem 'self'" {
		t.Errorf("script-free document: script-src-elem = %q, want \"script-src-elem 'self'\"", got)
	}

	// No CSP header (not routed through securityHeaders): leave untouched.
	rec3 := httptest.NewRecorder()
	ApplyDocumentScriptSrcElem(rec3, doc)
	if got := rec3.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("helper invented a CSP header out of nothing: %q", got)
	}

	// A CSP without a script-src-elem directive is left exactly as it was:
	// the helper rewrites the directive, it never introduces one.
	rec4 := httptest.NewRecorder()
	const noElem = "default-src 'self'; script-src 'self'"
	rec4.Header().Set("Content-Security-Policy", noElem)
	ApplyDocumentScriptSrcElem(rec4, doc)
	if got := rec4.Header().Get("Content-Security-Policy"); got != noElem {
		t.Errorf("CSP without script-src-elem was modified: %q, want %q", got, noElem)
	}
}
