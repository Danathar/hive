package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for kubestellar/hive#5704: /api/contribute/activity accepted a `limit`
// query parameter and ignored it. `limit=5` and `limit=300` both returned 50.
//
// The damage is not the wrong count, it is that the caller cannot SEE it. The
// response carried no total, no truncation flag and no cursor, so asking for
// 300 and receiving 50 was indistinguishable from "there were only 50 events".
// That produced a confidently wrong conclusion on #5701, where one
// contributor's events were counted over what looked like a full day and were
// in fact a 50-event window shared with every other contributor on the hive.

// activityRequest drives the real handler through the real mux and decodes the
// envelope, so what is asserted is the wire response rather than a helper's
// return value.
func activityRequest(t *testing.T, s *Server, query string) (entries []ActivityEntry, retained, capacity int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/activity"+query, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %q: status %d, body %s", query, w.Code, w.Body.String())
	}
	var body struct {
		Activity []ActivityEntry `json:"activity"`
		Retained int             `json:"retained"`
		Capacity int             `json:"capacity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v (body %s)", query, err, w.Body.String())
	}
	return body.Activity, body.Retained, body.Capacity
}

// seedActivity fills the feed with distinguishable events. Callers that want a
// FULL ring add more than maxActivityEntries on purpose: "the ring is full" is
// one of the three states the response has to let a caller tell apart.
func seedActivity(hub *ContributeWSHub, n int) {
	for i := 0; i < n; i++ {
		hub.addActivity(fmt.Sprintf("user%03d", i), "picked up", "scanner",
			"claude", "sonnet", "", fmt.Sprintf("repo#%d", i))
	}
}

// The reported bug, exactly.
func TestContributeActivity_HonoursLimit(t *testing.T) {
	_, s := covK2Hub(t)
	seedActivity(s.contributeHub, maxActivityEntries+20)

	got, retained, capacity := activityRequest(t, s, "?limit=5")
	if len(got) != 5 {
		t.Errorf("#5704: limit=5 returned %d events, want 5", len(got))
	}
	if retained != maxActivityEntries {
		t.Errorf("retained = %d, want the full ring (%d)", retained, maxActivityEntries)
	}
	if capacity != maxActivityEntries {
		t.Errorf("capacity = %d, want %d", capacity, maxActivityEntries)
	}
	if len(got) >= retained {
		t.Errorf("a truncated response must report a larger retained count: %d vs %d",
			len(got), retained)
	}

	big, retainedBig, _ := activityRequest(t, s, "?limit=300")
	if len(big) != maxActivityEntries {
		t.Errorf("limit=300 returned %d, want the %d actually retained",
			len(big), maxActivityEntries)
	}
	if len(big) != retainedBig {
		t.Errorf("an unclamped response must report len(activity) == retained: %d vs %d",
			len(big), retainedBig)
	}
}

// limit takes the MOST RECENT events. "The last N" is the only sensible reading,
// and it is what the dashboard's own field log already assumes with
// acts.slice(-6) — returning the oldest N would silently break that view.
func TestContributeActivity_LimitTakesTheNewestEvents(t *testing.T) {
	_, s := covK2Hub(t)
	seedActivity(s.contributeHub, 10)

	all, _, _ := activityRequest(t, s, "")
	if len(all) != 10 {
		t.Fatalf("seeded 10 events, got %d", len(all))
	}
	got, _, _ := activityRequest(t, s, "?limit=3")
	if len(got) != 3 {
		t.Fatalf("limit=3 returned %d", len(got))
	}
	for i := range got {
		want := all[len(all)-3+i]
		if got[i].Username != want.Username || got[i].Task != want.Task {
			t.Errorf("position %d = %s/%s, want the tail entry %s/%s",
				i, got[i].Username, got[i].Task, want.Username, want.Task)
		}
	}
}

// Every existing caller omits the parameter, and none may change behaviour: the
// dashboard field log and the Operations poller both read the whole feed.
func TestContributeActivity_DefaultIsUnchanged(t *testing.T) {
	_, s := covK2Hub(t)
	seedActivity(s.contributeHub, 12)

	for _, q := range []string{"", "?limit=", "?limit=0", "?limit=-4", "?limit=abc", "?limit=1e3"} {
		got, retained, _ := activityRequest(t, s, q)
		if len(got) != 12 {
			t.Errorf("%q returned %d events, want all 12 — an absent or unusable "+
				"limit must behave exactly as before", q, len(got))
		}
		if retained != 12 {
			t.Errorf("%q: retained = %d, want 12", q, retained)
		}
	}
}

// The three states a caller must be able to tell apart. The array alone can
// distinguish none of them, which is the root of the #5704 report.
func TestContributeActivity_RetainedDistinguishesShortFeedFromFullRing(t *testing.T) {
	_, sYoung := covK2Hub(t)
	seedActivity(sYoung.contributeHub, 7)
	got, retained, capacity := activityRequest(t, sYoung, "")
	if len(got) != 7 || retained != 7 {
		t.Fatalf("young hive: %d events, retained %d, want 7/7", len(got), retained)
	}
	if retained >= capacity {
		t.Errorf("a short feed must report retained < capacity (%d/%d) so a caller "+
			"knows nothing has been evicted", retained, capacity)
	}

	_, sBusy := covK2Hub(t)
	seedActivity(sBusy.contributeHub, maxActivityEntries*3)
	_, retainedBusy, capacityBusy := activityRequest(t, sBusy, "")
	if retainedBusy != capacityBusy {
		t.Errorf("a full ring must report retained == capacity (%d/%d)",
			retainedBusy, capacityBusy)
	}
}

// An uninitialised hub answers the same shape, so a caller never has to
// special-case a missing envelope.
func TestContributeActivity_NoHubStillReportsTheShape(t *testing.T) {
	_, s := covK2Hub(t)
	s.contributeHub = nil

	got, retained, capacity := activityRequest(t, s, "?limit=5")
	if len(got) != 0 || retained != 0 {
		t.Errorf("no hub: %d events / retained %d, want 0/0", len(got), retained)
	}
	if capacity != maxActivityEntries {
		t.Errorf("capacity must still be reported, got %d", capacity)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, `"activity":[]`) {
		t.Errorf("an empty feed must marshal as [], not null: %s", body)
	}
}

// The pure helper, at its boundaries.
func TestLimitActivity(t *testing.T) {
	mk := func(n int) []ActivityEntry {
		out := make([]ActivityEntry, n)
		for i := range out {
			out[i] = ActivityEntry{Username: fmt.Sprintf("u%d", i)}
		}
		return out
	}
	cases := []struct {
		name  string
		in    int
		limit int
		want  int
		first string
	}{
		{"limit below length takes the tail", 10, 3, 3, "u7"},
		{"limit equal to length is everything", 10, 10, 10, "u0"},
		{"limit above length clamps", 10, 99, 10, "u0"},
		{"zero means everything", 10, 0, 10, "u0"},
		{"negative means everything", 10, -1, 10, "u0"},
		{"limit of one takes the newest", 10, 1, 1, "u9"},
		{"empty feed with a limit is empty", 0, 5, 0, ""},
	}
	for _, tc := range cases {
		got := limitActivity(mk(tc.in), tc.limit)
		if len(got) != tc.want {
			t.Errorf("%s: len = %d, want %d", tc.name, len(got), tc.want)
			continue
		}
		if tc.first != "" && got[0].Username != tc.first {
			t.Errorf("%s: first = %s, want %s", tc.name, got[0].Username, tc.first)
		}
		if got == nil {
			t.Errorf("%s: must not return nil", tc.name)
		}
	}
}
