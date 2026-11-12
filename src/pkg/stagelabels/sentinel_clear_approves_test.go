package stagelabels

import (
	"strings"
	"testing"
)

// The 085 comment names the shortcut only on a hive that turned
// sentinel.clear_counts_as_approval on, and only while the sentinel alert is
// open. Off, the actions read exactly as they did before the setting
// existed.
func TestDecisionActionsSentinelClearApproves(t *testing.T) {
	base := readyEvidence()
	base.Verdict = VerdictRequiresHuman
	base.Sentinel = true
	off := DecisionActions(RouteHuman(base))
	const sentinelStep = "\n- resolve the sentinel alert and have a maintainer clear its label."
	if !strings.Contains(off, "**What to do:**\n\n- a trusted merger approves") || !strings.HasSuffix(off, sentinelStep) {
		t.Fatalf("off: actions changed:\n%s", off)
	}
	if strings.Contains(off, "also approves this head") {
		t.Fatalf("off: the shortcut is offered:\n%s", off)
	}

	withSetting := base
	withSetting.SentinelClearApproves = true
	r := RouteHuman(withSetting)
	if !r.SentinelClearApproves {
		t.Fatalf("switch not carried onto the 085 route: %+v", r)
	}
	on := DecisionActions(r)
	added := "\n- on this hive a trusted merger who removes the sentinel label also approves this head (`" + ShortSHA(base.Head) + "`): the removal answers the reviewer's `requires_human` on it, so no separate approval is needed."
	if on != off+added {
		t.Fatalf("on: want the off text plus one step\n got %q\nwant %q", on, off+added)
	}
	if got := DecisionComment(r); !strings.Contains(got, added) {
		t.Fatalf("085 comment lacks the step:\n%s", got)
	}

	// The alert is not open: nothing to remove, so nothing is offered.
	cleared := withSetting
	cleared.Sentinel = false
	if r := RouteHuman(cleared); r.SentinelClearApproves || strings.Contains(DecisionActions(r), "sentinel") {
		t.Fatalf("shortcut offered with no sentinel alert open: %+v\n%s", r, DecisionActions(r))
	}
}
