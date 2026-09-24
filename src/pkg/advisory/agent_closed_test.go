package advisory

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// TestAgentClosedFindingIsNotReportedResolved is the #6262 regression test.
//
// The guide agent re-checks its own findings each cycle and `bd close`s the
// ones it judges fixed. On the FMA digest of 2026-09-24 it closed "dual-pods
// controller log-tail capture ... is undocumented" although no docs had
// changed, and the digest struck it through as "resolved Sep 24". A close the
// hive has no evidence for must not be presented as a resolution; a close the
// hive does have evidence for (here, a merged PR) still is.
func TestAgentClosedFindingIsNotReportedResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	const agentTitle = "dual-pods controller log-tail capture on stopped vLLM instances is undocumented in docs/dual-pods.md"
	agentClosed, err := store.Create(agentTitle, beads.TypeAdvisory, beads.PriorityMedium, "guide", "docs/dual-pods.md#launcher-based-pods")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(agentClosed.ID); err != nil { // what `bd close` does
		t.Fatal(err)
	}
	const fixedTitle = "pr-verifier workflow fails on every pull request"
	fixed, err := store.Create(fixedTitle, beads.TypeAdvisory, beads.PriorityMedium, "guide", "")
	if err != nil {
		t.Fatal(err)
	}
	if closed := ClosePRLinkedAdvisoryBeadsAt(map[string]*beads.Store{"guide": store}, "fix the pr-verifier workflow failing on every pull request", time.Now()); len(closed) != 1 || closed[0] != fixed.Title {
		t.Fatalf("PR-linked close = %v, want [%q]", closed, fixed.Title)
	}

	opts := DigestOptions{Org: "acme", PrimaryRepo: "widgets"}
	d := BuildDigestFromBeads(map[string]*beads.Store{"guide": store}, "busy", opts)
	if len(d.RecentlyResolved) != 2 {
		t.Fatalf("RecentlyResolved = %+v, want both closes listed", d.RecentlyResolved)
	}
	for _, r := range d.RecentlyResolved {
		if want := r.Title == agentTitle; r.AgentClosed != want {
			t.Errorf("%q AgentClosed = %v, want %v", r.Title, r.AgentClosed, want)
		}
	}

	md := FormatDigestMarkdown(d, opts)
	resolvedAt := strings.Index(md, "### ✅ Recently Resolved (1)")
	closedAt := strings.Index(md, "### ☑️ Recently Closed by Agents — Fix Not Verified (1)")
	if resolvedAt < 0 || closedAt < 0 {
		t.Fatalf("want one evidenced resolution and one unverified agent close as separate sections:\n%s", md)
	}
	if strings.Contains(md, "~~"+agentTitle+"~~") {
		t.Errorf("agent-closed finding is struck through as resolved:\n%s", md)
	}
	if !strings.Contains(md, "_closed by guide "+time.Now().Format("Jan 2")+" — fix not verified_") {
		t.Errorf("agent-closed finding is not captioned as unverified:\n%s", md)
	}
	if !strings.Contains(md, "~~"+fixedTitle+"~~") {
		t.Errorf("PR-linked resolution lost its resolved rendering:\n%s", md)
	}
}

// TestAllClearDigestDoesNotOverclaimAgentCloses guards the zero-open-findings
// rendering: with only agent closes behind it, "all previously reported
// findings are resolved" is the same false claim in a different place.
func TestAllClearDigestDoesNotOverclaimAgentCloses(t *testing.T) {
	d := &Digest{
		GeneratedAt:      time.Now(),
		RecentlyResolved: []ResolvedFinding{{Agent: "guide", Title: "gap", ClosedAt: time.Now(), AgentClosed: true}},
	}
	md := FormatDigestMarkdown(d, DigestOptions{})
	if strings.Contains(md, "all previously reported findings are resolved") {
		t.Errorf("all-clear digest claims resolution on agent closes alone:\n%s", md)
	}
	if !strings.Contains(md, "1 recently closed by agents without a verified fix") {
		t.Errorf("all-clear digest does not report the unverified close:\n%s", md)
	}
}
