package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

func sentinelSecurityBlock(t *testing.T, s *Server) map[string]any {
	t.Helper()
	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sn, ok := body["security"].(map[string]any)["sentinel"].(map[string]any)
	if !ok {
		t.Fatalf("sentinel block missing: %#v", body["security"])
	}
	return sn
}

// sentinel.clear_counts_as_approval is off by default, saved and read back
// through the Security endpoint, left alone by a save that does not name it,
// and its flips are named in the audit entry.
func TestGovernorSecuritySentinelClearCountsAsApprovalRoundTrip(t *testing.T) {
	s, deps := apiServer(t)
	if got, present := sentinelSecurityBlock(t, s)["clearCountsAsApproval"]; !present || got != false {
		t.Fatalf("default clearCountsAsApproval = %#v (present %v), want false", got, present)
	}
	if deps.Config.Sentinel.ClearCountsAsApproval {
		t.Fatal("setting on before anyone turned it on")
	}

	rec := doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"clearCountsAsApproval": true, "trustedAuthorsBlock": true}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if !deps.Config.Sentinel.ClearCountsAsApproval || sentinelSecurityBlock(t, s)["clearCountsAsApproval"] != true {
		t.Fatalf("setting not saved: %+v", deps.Config.Sentinel)
	}
	if detail := lastAuditDetail(s, "config_governor_security"); !strings.Contains(detail, "sentinel_clear_counts_as_approval=off → on") {
		t.Fatalf("audit entry does not name the flip: %q", detail)
	}

	// A save that only touches another Sentinel field keeps it on.
	rec = doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"maxActions": 7}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if sc := deps.Config.Sentinel; !sc.ClearCountsAsApproval || !sc.TrustedAuthorsBlock || sc.MaxActions != 7 {
		t.Fatalf("partial update clobbered fields: %+v", sc)
	}
	if detail := lastAuditDetail(s, "config_governor_security"); strings.Contains(detail, "sentinel_clear_counts_as_approval") {
		t.Fatalf("an unchanged setting was audited as a flip: %q", detail)
	}

	rec = doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"clearCountsAsApproval": false}})
	if rec.Code != http.StatusOK || deps.Config.Sentinel.ClearCountsAsApproval || !deps.Config.Sentinel.TrustedAuthorsBlock {
		t.Fatalf("turning it off: code=%d config=%+v", rec.Code, deps.Config.Sentinel)
	}
	if detail := lastAuditDetail(s, "config_governor_security"); !strings.Contains(detail, "sentinel_clear_counts_as_approval=on → off") {
		t.Fatalf("audit entry does not name the flip back: %q", detail)
	}
}

// The Security tab's toggle carries its YAML key, a help mark, and help text
// that states the trade-off in plain words.
func TestGovernorSecuritySentinelClearCountsAsApprovalToggleHelp(t *testing.T) {
	html := indexHTML(t)
	body := extractJSFunction(t, html, "renderGovSecurity")
	start := strings.Index(body, "sn.clearCountsAsApproval ? 'on' : ''")
	if start < 0 {
		t.Fatal("renderGovSecurity has no clear-counts-as-approval toggle")
	}
	row := body[start:]
	if end := strings.Index(row, `<div class="config-field-row">`); end > 0 {
		row = row[:end]
	}
	for _, want := range []string{
		"sn.clearCountsAsApproval ? 'on' : ''",
		`data-action="toggleSentinelClearCountsAsApproval"`,
		"yamlHint('sentinel.clear_counts_as_approval')",
		"settingHelpMark('operator-reference/sweeps-and-operations', 'suspicious-activity-alerts-sentinel',",
		"Default off",
		"removing the label approves the head for every question the reviewer raised, not only Sentinel's",
		"by a bot or by the PR's own author approves nothing",
		"new commits need a new decision",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("toggle row lacks %q:\n%s", want, row)
		}
	}
	for _, fn := range []string{"toggleSentinelClearCountsAsApproval"} {
		if !strings.Contains(html, "function "+fn+"(el)") || !strings.Contains(html, "case '"+fn+"': "+fn+"(el); break;") {
			t.Errorf("%s is not defined and dispatched", fn)
		}
	}
	if !strings.Contains(html, "markSentinelDirty('clearCountsAsApproval', isOn);") {
		t.Error("the toggle does not stage sentinel.clearCountsAsApproval for the security save")
	}
}
