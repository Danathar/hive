package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSidebarVersionChipHasExplicitAffordance(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="oc-version-chip"`,
		`role="button"`,
		`aria-expanded="false"`,
		`title="Version & upgrade details"`,
		`class="oc-version-chip-chevron"`,
		`.oc-version-chip[aria-expanded="false"] .oc-version-chip-chevron`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("version chip missing %q", want)
		}
	}
}

func TestSidebarVersionDetailsFitAndFallbackRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version details JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
function escapeHtml(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
function relativeAge(){ return '2h ago'; }
const window = {};
` + jsFunc(t, html, "versionDeliveryLabel") + "\n" +
		jsFunc(t, html, "versionTrackingLabel") + "\n" +
		jsFunc(t, html, "versionTrackingTooltip") + "\n" +
		jsFunc(t, html, "versionCompareURL") + "\n" +
		jsFunc(t, html, "versionStatusText") + "\n" +
		jsFunc(t, html, "versionLastUpgradeText") + "\n" +
		jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "versionDashHTML") + "\n" +
		jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionManagedSuffix") + "\n" +
		jsFunc(t, html, "versionTrackingSummary") + "\n" +
		jsFunc(t, html, "versionCadenceLabel") + "\n" +
		jsFunc(t, html, "versionStatusSummary") + "\n" +
		jsFunc(t, html, "renderVersionUpgradeAction") + "\n" +
		jsFunc(t, html, "renderVersionDetails") + `
const out = renderVersionDetails({hash:'07d99d8426ec7b5ae364e25abcdef0123456789', short:'07d99d8', branch:'v5', channel:'candidate', tracking:'floating', latestHash:'07d99d8426ec7b5ae364e25abcdef0123456789', target:{sha:'07d99d8426ec7b5ae364e25abcdef0123456789', short:'07d99d8', managedBy:'hub'}, autoUpdate:{state:'up_to_date', managedBy:'hub', enabled:true, detail:'ok'}, releaseStatus:{attempt:{state:'succeeded', completedAt:'2026-10-02T10:00:00Z'}}}, {deliveryLabel:'candidate (v5)', offeredUpgradeHash:'07d99d8426ec7b5ae364e25abcdef0123456789', offeredUpgradeShort:'07d99d8'});
if (!out.includes('>07d99d8<')) throw new Error('short sha missing: '+out);
if (!out.includes('title="07d99d8426ec7b5ae364e25abcdef0123456789"')) throw new Error('full sha title missing: '+out);
for (const row of ['Tracking','Status','Cadence']) if (!out.includes('>'+row+'</span>')) throw new Error('missing row '+row+': '+out);
const empty = renderVersionDetails({hash:'abcdef1234567890', short:'abcdef1', tracking:'unknown', autoUpdate:{state:'unknown'}}, {deliveryLabel:''});
if (!empty.includes('>—</strong>')) throw new Error('fallback dash missing: '+empty);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar version details failed: %v\n%s", err, out)
	}
}

func TestSidebarVersionUpgradeActionRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version upgrade JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
function escapeHtml(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
` + jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionCadenceLabel") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "renderVersionUpgradeAction") + `
let v = {hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222', short:'bbb2222', managedBy:'hub'}, autoUpdate:{enabled:true, managedBy:'hub'}, upgradePolicy:{schedule:'daily', schedule_hour:13, schedule_timezone:'America/New_York'}, deployment:{upgradeSupported:true, runtime:'kubernetes'}};
let out = renderVersionUpgradeAction(v, {offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222', offeredUpgradeLabel:'Hub target'});
if (!out.includes('Hub will upgrade automatically') || out.includes('data-action="gh27"')) throw new Error('hub-managed action wrong: '+out);
v = {hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222', short:'bbb2222'}, autoUpdate:{enabled:false, state:'disabled'}, deployment:{upgradeSupported:true, runtime:'kubernetes'}};
out = renderVersionUpgradeAction(v, {offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222', offeredUpgradeLabel:'target'});
if (!out.includes('Upgrade to bbb2222 →') || !out.includes('data-action="gh27"')) throw new Error('manual button missing: '+out);
v = {hash:'bbb2222', short:'bbb2222', target:{sha:'bbb2222', short:'bbb2222'}, autoUpdate:{state:'up_to_date'}, deployment:{upgradeSupported:true}};
out = renderVersionUpgradeAction(v, {offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222'});
if (!out.includes('Up to date ✓')) throw new Error('up-to-date state wrong: '+out);
v = {hash:'aaa1111', short:'aaa1111', target:{resolved:false}, autoUpdate:{state:'unknown'}, deployment:{upgradeSupported:true}};
out = renderVersionUpgradeAction(v, {offeredUpgradeHash:'', offeredUpgradeShort:''});
if (!out.includes('Checking…')) throw new Error('checking state wrong: '+out);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar version upgrade rules failed: %v\n%s", err, out)
	}
}
