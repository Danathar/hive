package hub

import (
	"encoding/json"
	"net/http"
	"sort"
)

// countryRollupEntry is one country's share of the roster.
//
// Code is the normalized ISO 3166-1 alpha-2 code; the flag glyph is NOT sent.
// The client derives it from the code with the same regional-indicator
// arithmetic every other render site uses (countryFlagHTML in the dashboard),
// so there is exactly one place that decides what a country looks like and a
// malformed code degrades to silence at the render site rather than shipping a
// broken glyph down the wire.
type countryRollupEntry struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// countryRollup is the whole answer: the ranked known countries and the count
// we could not place.
//
// Unknown has NO omitempty, deliberately. Zero unknown is a real and meaningful
// state ("we know where everyone is"), and it must be distinguishable from
// "this build forgot to send the field". Countries is likewise always present
// as an array — see buildCountryRollup for why it is never nil.
type countryRollup struct {
	Countries []countryRollupEntry `json:"countries"`
	Unknown   int                  `json:"unknown"`
	// Total is every user counted, known and unknown. Sent rather than left to
	// the client to re-add, so the percentage the UI draws and the counts it
	// lists can never disagree about the denominator.
	Total int `json:"total"`
}

// buildCountryRollup aggregates a user list into the rollup.
//
// Split out from the handler so the aggregation is testable without a server,
// and so the ordering rule lives in one readable place.
//
// Ordering: count DESCENDING, then code ASCENDING as the tie-break. The
// tie-break is not cosmetic — Go map iteration is randomized, so without it two
// countries with equal counts would swap places on every poll and the list
// would visibly shuffle while nothing changed.
//
// Countries is always a non-nil slice. A nil slice marshals to JSON `null`,
// and `null.length` throws in the browser where `[].length` is 0 — so an
// all-unknown population (the expected state on day one) would take out the
// whole rollup render rather than showing "0 known, N unknown". Allocating the
// empty slice is what makes the empty case boring.
func buildCountryRollup(users []SaaSUser) countryRollup {
	counts := make(map[string]int)
	unknown := 0
	for i := range users {
		// normalizeCountryCode is the single arbiter of "usable country" — the
		// same one the storage and render paths use. A legacy or hand-edited
		// record holding junk therefore counts as UNKNOWN here rather than
		// creating a phantom country of one, which keeps the rollup's total
		// equal to the roster size no matter what is on disk.
		if code := normalizeCountryCode(users[i].Country); code != "" {
			counts[code]++
			continue
		}
		unknown++
	}
	entries := make([]countryRollupEntry, 0, len(counts))
	for code, n := range counts {
		entries = append(entries, countryRollupEntry{Code: code, Count: n})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Code < entries[j].Code
	})
	return countryRollup{Countries: entries, Unknown: unknown, Total: len(users)}
}

// handleAdminUserCountries serves the rollup. Registered behind requireAdmin,
// so a non-admin gets the gate's 403 and no data at all — not an empty rollup,
// which would be indistinguishable from "nobody has a country yet" and would
// still confirm the roster size via Total.
func (s *HubServer) handleAdminUserCountries(w http.ResponseWriter, r *http.Request) {
	rollup := buildCountryRollup(listAllSaaSUsers())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rollup)
}
