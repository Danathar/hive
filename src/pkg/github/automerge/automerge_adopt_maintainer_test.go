package automerge

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#619: the author was a repository admin. The
// hive nudged the PR at 072, adopted it as contributor-offline 16 minutes
// later and then re-homed it, closing the admin's own PR. Adoption stands in
// for a quiet contributor relay, never for a maintainer.

func TestRepoMaintainerLevel(t *testing.T) {
	for _, tc := range []struct {
		permission, role string
		want             bool
	}{
		{"admin", "admin", true},
		{"write", "maintain", true},
		{"ADMIN", "", true},
		{"write", "write", false},
		{"write", "triage", false},
		{"read", "read", false},
		{"none", "", false},
	} {
		if got := repoMaintainerLevel(tc.permission, tc.role); got != tc.want {
			t.Errorf("repoMaintainerLevel(%q, %q) = %v, want %v", tc.permission, tc.role, got, tc.want)
		}
	}
}

func TestAdoptionSkipsRepositoryMaintainer(t *testing.T) {
	for _, role := range []string{"admin", "maintain"} {
		t.Run(role, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.permissions = map[string]string{"mallory": role}
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			// Past the grace, then two more sweeps inside one cache window.
			for _, step := range []time.Duration{2 * time.Hour, 10 * time.Minute, 10 * time.Minute} {
				h.clock.advance(step)
				h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
			}
			if n := adoptionComments(h.api); len(n) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
				t.Fatalf("maintainer's PR adopted: comments=%v pulls=%v refs=%v", n, h.adopt.pulls, h.adopt.refs)
			}
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.Stage != stagelabels.ReturnedToContributor || rec.AdoptedHead != "" {
				t.Fatalf("record: %+v", rec)
			}
			if h.adopt.permissionReads != 1 {
				t.Fatalf("permission read %d times, want 1 (cached)", h.adopt.permissionReads)
			}
		})
	}
}

func TestAdoptionFailsClosedOnPermissionError(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.adopt.failPermission = true
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("adopted although the permission read failed: %v", h.api.comments)
	}
	// The error is not cached: once the read works, a write-level author
	// is adopted as before.
	h.adopt.failPermission = false
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(adoptionComments(h.api)) != 1 {
		t.Fatalf("write-level author not adopted after the read recovered: %v", h.api.comments)
	}
}

// An in-place adoption taken before the author was known to maintain the
// repository is released on the next pass and never re-homed.
func TestInPlaceAdoptionOfMaintainerIsReleased(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() {
		t.Fatalf("setup: not adopted in place: %+v", rec)
	}

	// The author is (now) an admin; the cached write-level answer expires.
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "" || rec.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("maintainer's in-place adoption kept: %+v", rec)
	}
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("maintainer's PR re-homed: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
}
