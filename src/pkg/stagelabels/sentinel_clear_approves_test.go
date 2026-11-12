package stagelabels

import (
	"strings"
	"testing"
)

// The 085 comment names the shortcut only on a hive that turned
// sentinel.clear_counts_as_approval on, and only while the sentinel alert is
// open. Off, the actions are what they were before the setting existed: the
// step is one appended list item and nothing else in the text moves.
func TestDecisionActionsSentinelClearApproves(t *testing.T) {
	base := readyEvidence()
	base.Verdict = VerdictRequiresHuman
	base.Sentinel = true
	offRoute := RouteHuman(base)
	off := DecisionActions(offRoute)
	if offRoute.Stage != NeedsDecision || offRoute.SentinelClearApproves || !strings.Contains(off, "sentinel") {
		t.Fatalf("off: want an 085 route with a sentinel step and no switch: %+v\n%s", offRoute, off)
	}
	const sentence = "on this hive a trusted merger who removes the sentinel label also approves this head (`abc1234`): the removal answers the reviewer's `requires_human` on it, so no separate approval is needed"
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
	if !strings.HasSuffix(on, "\n- "+sentence+".") {
		t.Fatalf("on: the step must close the list as its own item:\n%s", on)
	}
	if strings.Count(on, "also approves this head") != 1 {
		t.Fatalf("on: the step must appear once:\n%s", on)
	}
	// When the off text is already a list, on is that list plus one item.
	if strings.Contains(off, "\n- ") && on != off+"\n- "+sentence+"." {
		t.Fatalf("on: want the off text plus one step\n got %q\nwant %q", on, off+"\n- "+sentence+".")
	}
	if got := DecisionComment(r); !strings.Contains(got, sentence) {
		t.Fatalf("085 comment lacks the step:\n%s", got)
	}

	// The alert is not open: nothing to remove, so nothing is offered.
	cleared := withSetting
	cleared.Sentinel = false
	if r := RouteHuman(cleared); r.SentinelClearApproves || strings.Contains(DecisionActions(r), "also approves this head") {
		t.Fatalf("shortcut offered with no sentinel alert open: %+v\n%s", r, DecisionActions(r))
	}
}
