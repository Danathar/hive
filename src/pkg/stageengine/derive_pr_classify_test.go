package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A PR held by an issue it closes owes a decision on that issue: every lane
// routes the linked-issue reasons to 085 as a human hand-off. The
// fail-closed read error is not a decision and claims no stage.
func TestClassifyStageLinkedIssueReasons(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		lane      string
		wantStage stagelabels.Stage
		wantHuman bool
	}{
		{reason: prstage.LinkedIssueReason(prstage.LinkedIssueReasonHoldPrefix, 551, "hold"), lane: stageLaneSelf, wantStage: stagelabels.NeedsDecision, wantHuman: true},
		{reason: prstage.LinkedIssueReason(prstage.LinkedIssueReasonHoldPrefix, 551, "hive-pause/hive-a"), lane: stageLaneTrusted, wantStage: stagelabels.NeedsDecision, wantHuman: true},
		{reason: prstage.LinkedIssueReason(prstage.LinkedIssueReasonNeedsDecisionPrefix, 551, "stage/045-needs-decision"), lane: stageLaneContributor, wantStage: stagelabels.NeedsDecision, wantHuman: true},
		{reason: prstage.LinkedIssueReason(prstage.LinkedIssueReasonNeedsDecisionPrefix, 551, "needs-human"), lane: "queued", wantStage: stagelabels.NeedsDecision, wantHuman: true},
		{reason: prstage.LinkedIssueReasonCheck, lane: stageLaneSelf, wantStage: stagelabels.None},
		{reason: "linked-issue:hold:551", lane: stageLaneSelf, wantStage: stagelabels.None},
	} {
		t.Run(tc.lane+"/"+tc.reason, func(t *testing.T) {
			v := ClassifyStage(stageObservation{Lane: tc.lane, Reason: tc.reason, HeadSHA: "abc1234"})
			if v.Stage != tc.wantStage || v.Human != tc.wantHuman {
				t.Fatalf("ClassifyStage(%q) = stage %v human %v, want %v / %v", tc.reason, v.Stage, v.Human, tc.wantStage, tc.wantHuman)
			}
			if tc.wantStage == stagelabels.NeedsDecision && v.Rank != stageRankOwned {
				t.Fatalf("rank = %d, want owned", v.Rank)
			}
		})
	}
}

// needs-rebase on the PR still sends it to the author first, as for every
// other decision reason.
func TestClassifyStageLinkedIssueYieldsToRebase(t *testing.T) {
	v := ClassifyStage(stageObservation{Lane: stageLaneSelf, Reason: prstage.LinkedIssueReason(prstage.LinkedIssueReasonHoldPrefix, 551, "hold"), Labels: []string{"needs-rebase"}, HeadSHA: "abc1234"})
	if v.Stage != stagelabels.Fixing {
		t.Fatalf("stage = %v, want %v", v.Stage, stagelabels.Fixing)
	}
}
