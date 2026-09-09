package advisory

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestDigestFindingStateSerialization(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Second)
	seen := time.Now().UTC().Add(-18 * time.Minute).Truncate(time.Second)
	b := newPrunableBead(t, store, "cleanup paths lack coverage", seen)
	if err := store.Update(b.ID, func(b *beads.Bead) {
		b.CreatedAt.Time = created
		b.Notes = "computed at " + provenanceOfOne + "; hold-gated PR #224"
		b.ExternalRef = "contrib/aib"
	}); err != nil {
		t.Fatal(err)
	}
	newPrunableBead(t, store, "legacy unlinked finding", time.Time{})
	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		Snapshot:   &Snapshot{Owner: "org", Repo: "repo", SHA: analyzedAtSHA},
		VerifyPath: func(string) bool { return false },
	})
	checked := time.Now().Truncate(time.Second)
	ResolveLinkedWork(d, "org", "repo", func(string, string, int) (LinkedWork, bool) {
		return LinkedWork{Kind: "pr", State: "OPEN", CheckedAt: checked}, true
	})
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ByAgent map[string][]map[string]json.RawMessage `json:"by_agent"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for _, f := range wire.ByAgent["quality"] {
		if string(f["title"]) == `"legacy unlinked finding"` {
			if _, ok := f["last_seen_at"]; ok {
				t.Fatal("legacy finding must not invent a last-seen time")
			}
			if _, ok := f["linked_work"]; ok {
				t.Fatal("unlinked finding must omit linked_work")
			}
			continue
		}
		for key, want := range map[string]string{
			"timestamp":        fmt.Sprintf("%q", created.Format(time.RFC3339)),
			"last_seen_at":     fmt.Sprintf("%q", seen.Format(time.RFC3339)),
			"provenance_stale": "true", "path_stale": "true",
		} {
			if string(f[key]) != want {
				t.Errorf("%s = %s, want %s", key, f[key], want)
			}
		}
		var work []LinkedWork
		if err := json.Unmarshal(f["linked_work"], &work); err != nil {
			t.Fatal(err)
		}
		if len(work) != 1 || work[0].State != "OPEN" || work[0].Kind != "pr" ||
			work[0].URL != "https://github.com/org/repo/pull/224" || !work[0].CheckedAt.Equal(checked) {
			t.Fatalf("unexpected serialized linked work: %+v", work)
		}
	}
	var roundTrip Digest
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.TotalCount != 2 || statusOf(t, store, b.ID) != beads.StatusOpen {
		t.Fatal("display enrichment changed the finding lifecycle/count")
	}
}

func TestFindingWorkReferences(t *testing.T) {
	cases := []struct {
		name        string
		f           Finding
		owner, repo string
		want        []workRef
	}{
		{"bare prose", Finding{Detail: "Filed issue #208, hold-gated PR #209"}, "org", "repo", []workRef{{"org", "repo", 208}, {"org", "repo", 209}}},
		{"external title hint", Finding{File: "gh-7", Title: "other#7 missing checks"}, "org", "repo", []workRef{{"org", "other", 7}}},
		{"source prefix", Finding{File: "gh-Danathar/atomic-image-builder#208"}, "org", "repo", []workRef{{"Danathar", "atomic-image-builder", 208}}},
		{"qualified no context", Finding{Title: "owner/other#7", Detail: "#8"}, "", "", []workRef{{"owner", "other", 7}}},
		{"no context", Finding{File: "gh-7", Detail: "#8"}, "", "", nil},
		{"urls", Finding{File: "https://github.com/owner/other/pull/7#123", Detail: "https://github.com/owner/other/issues/8"}, "org", "repo", []workRef{{"owner", "other", 7}, {"owner", "other", 8}}},
		{"deduplicate case", Finding{File: "Owner/Repo#7", Title: "owner/repo#7", Detail: "https://github.com/OWNER/REPO/pull/7"}, "org", "repo", []workRef{{"Owner", "Repo", 7}}},
		{"no inferred links", Finding{Title: "cleanup paths lack coverage", Detail: "see https://example.com/owner/repo#7 or PR 42; color #abcdef"}, "org", "repo", nil},
		{"invalid numbers", Finding{Detail: "#0 #99999999999999999999999"}, "org", "repo", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingWorkRefs(tc.f, tc.owner, tc.repo); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("refs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveLinkedWorkStatesAndFailures(t *testing.T) {
	for _, state := range []string{"OPEN", "MERGED", "CLOSED", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			d := &Digest{TotalCount: 2, ByAgent: map[string][]Finding{
				"a": {{Title: "one", Detail: "PR #7", ProvenanceStale: true, ProvenanceSHA: provenanceOfOne}},
				"b": {{Title: "two", Detail: "PR #7", CachedReplays: 3}},
			}}
			calls := 0
			ResolveLinkedWork(d, "org", "repo", func(string, string, int) (LinkedWork, bool) {
				calls++
				return LinkedWork{Kind: "pr", State: state}, state != "UNKNOWN"
			})
			if calls != 1 {
				t.Fatalf("shared reference looked up %d times, want 1", calls)
			}
			for _, fs := range d.ByAgent {
				if len(fs) != 1 || len(fs[0].LinkedWork) != 1 || fs[0].LinkedWork[0].State != state {
					t.Fatalf("finding/reference lost or wrong state: %+v", fs)
				}
			}
			if d.TotalCount != 2 || len(d.RecentlyResolved) != 0 || !d.ByAgent["a"][0].ProvenanceStale || d.ByAgent["b"][0].CachedReplays != 3 {
				t.Fatal("reference display altered counts, retirement, or evidence state")
			}
		})
	}
}

func TestResolveLinkedWorkBudgetAndUnavailable(t *testing.T) {
	var detail strings.Builder
	for i := 1; i <= 260; i++ {
		fmt.Fprintf(&detail, " #%d", i)
	}
	d := &Digest{ByAgent: map[string][]Finding{"a": {{Detail: detail.String()}}}}
	calls := 0
	ResolveLinkedWork(d, "org", "repo", func(string, string, int) (LinkedWork, bool) {
		calls++
		return LinkedWork{Kind: "issue", State: "OPEN"}, true
	})
	work := d.ByAgent["a"][0].LinkedWork
	if calls != 250 || len(work) != 260 || work[249].State != "OPEN" || work[250].State != "UNKNOWN" {
		t.Fatalf("lookup budget not respected: calls=%d refs=%d", calls, len(work))
	}
	ResolveLinkedWork(d, "org", "repo", nil)
	if d.ByAgent["a"][0].LinkedWork[0].State != "UNKNOWN" {
		t.Fatal("missing resolver retained previously known state")
	}
	ResolveLinkedWork(nil, "org", "repo", nil)
}
