package dashboard

import (
	"strings"
	"testing"
)

// #6945: the Operations tab's work panel was titled "My work" while rendering
// the in-flight work of EVERY connected clanker to every visitor, anonymous ones
// included. /api/contribute/fleet is public, FleetSnapshot takes no viewer, and
// renderWork filtered on status alone — no identity was consulted at any layer.
//
// The data was not wrong (a fleet-wide view is the operator's view of their
// hive). The title was: it used the same possessive voice as "Your contribution"
// directly below it, which genuinely is per-viewer. So the panel is now called
// what it holds, and the scope the old title promised is a real filter.
//
// These tests pin both halves plus the identity plumbing the "Mine" scope needs,
// in the strings.Contains style the other Operations-page tests use.

// TestFleetWorkPanelRenamed proves the possessive title is gone from the panel
// that was never scoped to the viewer. The count badge and list ids are
// deliberately untouched, so the rename cannot have moved the hydration targets.
func TestFleetWorkPanelRenamed(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, `<h3>Fleet work</h3>`) {
		t.Error("work panel is not titled Fleet work")
	}
	if strings.Contains(body, `<h3>My work</h3>`) {
		t.Error(`the possessive "My work" heading is still rendered over fleet-wide contents`)
	}
	if !strings.Contains(body, `<h3>Fleet work</h3><span class="ops-card-count" id="work-count"></span>`) {
		t.Error("the rename disturbed the #work-count badge markup")
	}
	// "Your contribution" keeps its possessive voice — it earns it. The point of
	// the rename is that the two adjacent panels no longer claim the same scope.
	if !strings.Contains(body, `<h3>Your contribution</h3>`) {
		t.Error(`"Your contribution" heading regressed`)
	}
}

// TestFleetWorkScopeChipsRendered proves the All/Mine axis exists in the markup
// and is a SEPARATE class from the status filters. Folding the scope chips into
// .ops-filter would make picking "Mine" silently reset Active/Review/Done,
// because that handler deactivates every .ops-filter on the page.
func TestFleetWorkScopeChipsRendered(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`class="ops-scope active" data-scope="all"`,
		`class="ops-scope" data-scope="mine"`,
		`.ops-filter,.ops-scope{`, // shared chip styling, one rule
		`.ops-filter.active,.ops-scope.active{`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered contribute page missing scope-filter marker %q", want)
		}
	}
	if strings.Contains(body, `class="ops-filter" data-scope=`) ||
		strings.Contains(body, `class="ops-scope" data-filter=`) {
		t.Error("scope and status chips share a class; one handler will clobber the other's selection")
	}
	// The status filters are untouched: four chips, same values.
	for _, want := range []string{`data-filter="all"`, `data-filter="active"`, `data-filter="review"`, `data-filter="done"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status filter %q regressed", want)
		}
	}
}

// TestFleetWorkScopeAxesAreIndependent proves the two handlers only ever clear
// their OWN axis — the bug the separate classes exist to prevent.
func TestFleetWorkScopeAxesAreIndependent(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "document.querySelectorAll('.ops-scope').forEach(function(f){f.addEventListener('click',function(){") {
		t.Fatal("scope chips have no click handler")
	}
	// Each sweep must name its own class only.
	if strings.Contains(body, "currentScope=f.getAttribute('data-scope');") == false {
		t.Error("scope handler does not record the chosen scope")
	}
	scopeHandler := between(t, body, "document.querySelectorAll('.ops-scope').forEach(", "renderWork(lastWork);")
	if strings.Contains(scopeHandler, ".ops-filter") {
		t.Error("the scope handler also sweeps .ops-filter — picking Mine would reset the status filter")
	}
	statusHandler := between(t, body, "document.querySelectorAll('.ops-filter').forEach(", "renderWork(lastWork);")
	if strings.Contains(statusHandler, ".ops-scope") {
		t.Error("the status handler also sweeps .ops-scope — picking Active would reset the scope")
	}
}

// TestFleetWorkScopeMatchesViewerCaseInsensitively pins the comparison itself. It
// must be the same one lbRow makes for the standings self-highlight: the login a
// session hands us and the case a profile registered under are one account spelled
// two ways, so a case-sensitive test would hide a contributor's own work from them.
func TestFleetWorkScopeMatchesViewerCaseInsensitively(t *testing.T) {
	body := renderContributePage(t)

	fn := jsFunc(t, body, "workMatchesScope")
	if !strings.Contains(fn, "toLowerCase()") || strings.Count(fn, "toLowerCase()") < 2 {
		t.Errorf("workMatchesScope does not compare case-insensitively; body was:\n%s", fn)
	}
	if !strings.Contains(fn, "ccMeUsername") {
		t.Errorf("workMatchesScope does not key off the resolved viewer; body was:\n%s", fn)
	}
	if !strings.Contains(fn, "github_username") {
		t.Errorf("workMatchesScope does not read the row's contributor; body was:\n%s", fn)
	}
	// Anonymous matches nothing rather than everything: a "Mine" list that fell
	// back to fleet-wide would reproduce the exact defect this issue reports.
	if !strings.Contains(fn, "if(!ccMeUsername)return false;") {
		t.Errorf("anonymous viewers do not match zero rows under Mine; body was:\n%s", fn)
	}
	// 'all' short-circuits, so the default scope cannot drop rows.
	if !strings.Contains(fn, "if(currentScope!=='mine')return true;") {
		t.Errorf("the All scope does not short-circuit; body was:\n%s", fn)
	}
}

// TestFleetWorkScopeAppliesToBothSources proves the scope covers the "Done" rows
// too. "Done" is not sourced from the in-flight array (it comes from completed
// activity events), so a filter applied inside the status branch would have
// scoped Active and Review while leaving Done fleet-wide.
func TestFleetWorkScopeAppliesToBothSources(t *testing.T) {
	body := renderContributePage(t)

	fn := jsFunc(t, body, "renderWork")
	if !strings.Contains(fn, "shown=shown.filter(workMatchesScope);") {
		t.Errorf("renderWork does not apply the scope to the assembled list; body was:\n%s", fn)
	}
	// The scope filter must come AFTER the done/in-flight branch, or it only ever
	// sees one of the two sources.
	branch := strings.Index(fn, "else{shown=list.filter(workMatchesFilter);}")
	scope := strings.Index(fn, "shown=shown.filter(workMatchesScope);")
	if branch < 0 || scope < 0 || scope < branch {
		t.Errorf("scope filter is not applied after the status branch (branch=%d scope=%d)", branch, scope)
	}
	// ccCompletedWorkItems carries the field the scope reads, or "Mine + Done" is
	// empty by construction rather than by fact.
	if !strings.Contains(jsFunc(t, body, "ccCompletedWorkItems"), "github_username:e.username") {
		t.Error("completed rows carry no github_username, so the Mine scope cannot match them")
	}
}

// TestFleetWorkEmptyMineExplainsItself proves an empty "Mine" says why it is
// empty. For an anonymous viewer it is not "no work" at all — it is "we don't
// know who you are" — so that case gets the Profile tab's .me-signin treatment
// rather than a fleet-wide statement about a list scoped to nobody.
func TestFleetWorkEmptyMineExplainsItself(t *testing.T) {
	body := renderContributePage(t)

	fn := jsFunc(t, body, "renderWork")
	if !strings.Contains(fn, "if(currentScope==='mine'&&!ccMeUsername){") {
		t.Errorf("the anonymous Mine empty state is not distinguished; body was:\n%s", fn)
	}
	for _, want := range []string{
		`<div class="me-signin">`, // the Profile tab's class, reused not reinvented
		"Sign in with GitHub",
		"All contributors", // names the way back to the full view
		"You have no work in flight",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("renderWork's empty states missing %q", want)
		}
	}
	// The fleet-wide wording survives for the All scope — this issue did not ask
	// for that message to change.
	if !strings.Contains(fn, "'No work items in flight'") {
		t.Error("the fleet-wide empty state wording was lost")
	}
}

// TestOpsTabResolvesViewer is the plumbing the Mine scope depends on. Every
// pre-existing setter of ccMeUsername hangs off a DIFFERENT tab — loadMeStanding
// and loadMeCard (Rankings, Profile) and ccLoadMine, which only assigns on a 2xx
// — so a visitor who opens Operations directly and stays there had no resolved
// identity, and "Mine" would have hidden a signed-in contributor's own work.
func TestOpsTabResolvesViewer(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "function ccResolveViewer(){") {
		t.Fatal("ccResolveViewer is missing")
	}
	fn := jsFunc(t, body, "ccResolveViewer")
	if !strings.Contains(fn, "/api/gh-user-auth/status") {
		t.Error("ccResolveViewer does not read the same identity source the Me card uses")
	}
	if !strings.Contains(fn, "ccMeUsername=who;") {
		t.Error("ccResolveViewer does not set ccMeUsername")
	}
	if !strings.Contains(fn, "renderWork(lastWork)") {
		t.Error("ccResolveViewer does not repaint the work list once identity lands")
	}
	if !strings.Contains(fn, "ccViewerResolved") {
		t.Error("ccResolveViewer is not once-only; it would refetch on every call")
	}
	// Called from the Operations tab boot, and guarded like its siblings there: a
	// throwing identity lookup must not leave the fleet panels on "Loading…".
	if !strings.Contains(body, "try{ccResolveViewer();}catch(e){console.error('ccResolveViewer failed',e);}") {
		t.Error("ccResolveViewer is not invoked (guarded) from the Operations tab boot")
	}
}

// between returns the slice of s from the first occurrence of `from` up to the
// first `to` that follows it, so a test can assert about one handler body rather
// than about the whole document.
func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("marker %q not found", from)
	}
	rest := s[i:]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("terminator %q not found after %q", to, from)
	}
	return rest[:j]
}
