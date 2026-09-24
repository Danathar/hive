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
	closedAt := strings.Index(md, "### ☑️ Recently Closed — Fix Not Verified (1)")
	if resolvedAt < 0 || closedAt < 0 {
		t.Fatalf("want one evidenced resolution and one unverified agent close as separate sections:\n%s", md)
	}
	if strings.Contains(md, "~~"+agentTitle+"~~") {
		t.Errorf("agent-closed finding is struck through as resolved:\n%s", md)
	}
	if !strings.Contains(md, "_guide — closed "+time.Now().Format("Jan 2")+", fix not verified_") {
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
	if !strings.Contains(md, "1 recently closed without a verified fix") {
		t.Errorf("all-clear digest does not report the unverified close:\n%s", md)
	}
}

// TestCappedUnverifiedClosesAreNotSummarizedAsResolved: the changelog cap
// keeps the newest entries, so older bare closes can all fall past it. The
// hidden remainder must not then be announced as "resolved", and the
// zero-findings summary must not claim everything was resolved.
func TestCappedUnverifiedClosesAreNotSummarizedAsResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	for i := range 3 {
		b, err := store.Create("bare close "+string(rune('a'+i)), beads.TypeAdvisory, beads.PriorityHigh, "guide", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		// Older than every evidence-backed close below, so the cap drops these.
		if err := store.SetMetadata(b.ID, resolvedAtMetadataKey, formatResolvedAt(time.Now().Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	seedResolved(t, store, "guide", 2)

	opts := DigestOptions{MaxFindings: 2}
	d := BuildDigestFromBeads(map[string]*beads.Store{"guide": store}, "busy", opts)
	if d.ResolvedOverflowCount != 3 || d.UnverifiedOverflowCount != 3 {
		t.Fatalf("overflow = %d (unverified %d), want 3 (3)", d.ResolvedOverflowCount, d.UnverifiedOverflowCount)
	}
	md := FormatDigestMarkdown(d, opts)
	if strings.Contains(md, "all previously reported findings are resolved") {
		t.Errorf("summary claims all resolved while bare closes sit past the cap:\n%s", md)
	}
	if !strings.Contains(md, "…plus 3 more closed without a verified fix") {
		t.Errorf("hidden bare closes are not labelled unverified:\n%s", md)
	}
}

// TestGuideFindingNotPRLinkedByFMATitles pins that the #6262 guide finding was
// NOT retired by title-similarity PR auto-close: none of the FMA PRs merged
// around the false "resolved Sep 24" clears prLinkThreshold against it, so the
// close came from the agent and belongs in the unverified section.
func TestGuideFindingNotPRLinkedByFMATitles(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	const title = "dual-pods controller log-tail capture on stopped vLLM instances is undocumented in docs/dual-pods.md and docs/launcher.md"
	if _, err := store.Create(title, beads.TypeAdvisory, beads.PriorityMedium, "guide", "docs/dual-pods.md#launcher-based-pods"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}
	for _, pr := range []string{
		"Tweak dependency bump review instructions",
		"deps(actions): bump docker/setup-buildx-action from 4.3.0 to 4.4.1",
		"deps(actions): bump docker/setup-qemu-action from 4.3.0 to 4.4.0",
		"deps(actions): bump docker/build-push-action from 7.3.0 to 7.4.0",
		"Correct testing of releases wrt --debug-gpu-memory",
	} {
		if closed := ClosePRLinkedAdvisoryBeads(stores, pr); len(closed) != 0 {
			t.Errorf("PR %q closed %v; the guide finding shares no fix with it", pr, closed)
		}
	}
}
