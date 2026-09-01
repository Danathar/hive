package beads

import "testing"

// These are CHARACTERIZATION tests: they pin down what `bd ready` + `bd update
// --claim` does today, because the step-3 handoff evaluation for RFC #4002
// (src/docs/design/agent-turn-handoff.md) rests on it. That issue's hard
// problem 2 says cross-process handoff "must reuse the existing atomic
// offer->claim path" rather than parallel it. The path exists; the atomicity
// does not. Nothing here asserts that today's behaviour is desirable — only
// that a handoff design may not assume otherwise.
//
// Each test skips with a rewrite instruction if the guarantee it says is
// missing ever arrives, so a later compare-and-set lands as a signal to update
// the evaluation rather than as an unexplained red build.

// TestClaimDoesNotRejectAnAlreadyClaimedBead pins the mutual-exclusion gap.
// Claim writes StatusInProgress unconditionally through Update, so the second
// claimant is told it succeeded. The cross-process flock in xproc_lock.go
// serializes the two WRITES; it does not make the second one a no-op, because
// nothing compares against the prior status.
func TestClaimDoesNotRejectAnAlreadyClaimedBead(t *testing.T) {
	dir := t.TempDir()

	// Two stores over one directory stand in for two processes — the shape a
	// handoff creates when a replacement adopts a task whose previous holder
	// has not actually exited yet.
	first, err := NewStore(dir)
	if err != nil {
		t.Fatalf("creating first store: %v", err)
	}
	bead, err := first.Create("resume the interrupted turn", TypeTask, PriorityHigh, "contributor", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	second, err := NewStore(dir)
	if err != nil {
		t.Fatalf("creating second store: %v", err)
	}

	if err := first.Claim(bead.ID); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := second.Claim(bead.ID); err != nil {
		t.Skipf("second claim was rejected (%v) — Claim has become a compare-and-set; "+
			"rewrite this test as the exclusion guarantee it now provides", err)
	}

	// Both callers were told they hold the task. That is the window a
	// queue-based handoff would inherit if it claimed through this path.
	after, err := second.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading bead back: %v", err)
	}
	if after.Status != StatusInProgress {
		t.Fatalf("status = %q, want %q", after.Status, StatusInProgress)
	}
}

// TestClaimRecordsNoClaimant pins the second half of the gap: a claim leaves no
// trace of WHO claimed. Actor is set at Create and describes who the bead is
// addressed to, not who holds it now, and Claim does not touch it.
//
// This is why re-entry cannot currently distinguish "I already hold this,
// resume it" from "somebody else holds this, leave it alone" — the distinction
// a handoff lease has to make, and the reason the step-3 evaluation puts an
// owner and a lease in the turn envelope rather than reading ownership off the
// bead.
func TestClaimRecordsNoClaimant(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	bead, err := store.Create("resume the interrupted turn", TypeTask, PriorityHigh, "contributor", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := store.Claim(bead.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	claimed, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading bead back: %v", err)
	}
	if claimed.Actor != "contributor" {
		t.Fatalf("Actor = %q, want it unchanged at %q — Actor is the addressee, and a "+
			"claim that rewrote it would destroy the assignment", claimed.Actor, "contributor")
	}
	for _, key := range []string{"claimed_by", "claim_holder", "lease_owner", "holder"} {
		if got := claimed.Meta(key); got != "" {
			t.Skipf("metadata %q = %q — a claimant is now recorded; "+
				"the step-3 evaluation's premise needs revisiting", key, got)
		}
	}
}

// TestReadyOffersTheSameBeadToRepeatedReaders pins the offer side. Ready is a
// pure read with no reservation, so polling it twice — as two processes sharing
// one actor identity do — hands the same task out twice. Any exclusion would
// have to come from the claim, which the first test shows does not provide it.
func TestReadyOffersTheSameBeadToRepeatedReaders(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	bead, err := store.Create("resume the interrupted turn", TypeTask, PriorityHigh, "contributor", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	other, err := NewStore(dir)
	if err != nil {
		t.Fatalf("creating second store: %v", err)
	}

	readers := map[string]*Store{"first reader": store, "second reader": other}
	for name, s := range readers {
		found := false
		for _, b := range s.Ready("contributor") {
			if b.ID == bead.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: bead %s absent from Ready — the offer side has gained a "+
				"reservation and the step-3 evaluation needs revisiting", name, bead.ID)
		}
	}
}
