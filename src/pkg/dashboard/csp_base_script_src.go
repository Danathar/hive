package dashboard

// csp_base_script_src.go — the dashboard-side half of the CSP script-src
// machinery. The pure hash/extraction helpers live in pkg/dashboard/webstatic
// (extracted under #5565); this file keeps only the startup composition that
// depends on dashboard-owned bytes: the embedded SPA (staticFS) and the
// device-flow login page const.

import (
	"io/fs"
	"sync"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

var (
	baseScriptSrcElemOnce    sync.Once
	baseScriptSrcElemSources string

	brandedIndexMu sync.RWMutex
	brandedIndex   []byte
)

// setBrandedIndex records the index document AS SERVED, so CSP hashes are
// computed over the same bytes the browser receives.
//
// This matters because branding can rewrite inline SCRIPT content, not just
// markup: the Getting Started flyer builds its DOM from a JavaScript string
// literal that contains `<span class="wb-bee">&#x1F41D;</span>`. Replacing the
// mark there changes a script's bytes, and hashes taken from the embedded
// document would no longer authorise it — CSP would block the flyer on a
// branded hive. Must be called before the first request is served.
func setBrandedIndex(doc []byte) {
	brandedIndexMu.Lock()
	brandedIndex = append([]byte(nil), doc...)
	// Invalidate any previously memoised source list. baseScriptSrcElem uses a
	// sync.Once, so without this the hashes depend on whether anything happened
	// to ask for them before the document was built — in production Start()
	// builds it before serving, but that is an ordering assumption rather than
	// a guarantee, and it is exactly the kind of thing that works until it
	// silently does not. Safe because this runs at startup, before any request.
	baseScriptSrcElemOnce = sync.Once{}
	brandedIndexMu.Unlock()
}

// baseScriptSrcElem returns the startup-computed script-src-elem source list
// covering the two documents whose bytes are fixed for the life of the
// process: the embedded SPA (static/index.html, served verbatim by both
// webstatic.IndexDocument and the plain file server) and the device-flow login
// page (a const, served to any unauthenticated browser path). Computed once,
// like the #3863 gzip/ETag precomputation it must stay compatible with.
func baseScriptSrcElem() string {
	baseScriptSrcElemOnce.Do(func() {
		var docs []byte
		brandedIndexMu.RLock()
		branded := brandedIndex
		brandedIndexMu.RUnlock()
		if len(branded) > 0 {
			docs = append(docs, branded...)
		} else if raw, err := fs.ReadFile(staticFS, "static/index.html"); err == nil {
			docs = append(docs, raw...)
		}
		docs = append(docs, []byte(loginPage)...)
		baseScriptSrcElemSources = webstatic.ScriptSrcElemSources(docs)
	})
	return baseScriptSrcElemSources
}
