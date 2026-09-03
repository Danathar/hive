package hub

import (
	"strings"
	"testing"
)

// TestFleetRemediationHintPinned pins the /fleet remediation line (#5577):
// the verdict's one-line "do this" renders under the WHY chip on non-green
// rows, with a short link when the hub resolved one. Invariants:
//
//  1. Rendered only for problem/warning rows and only from
//     healthVerdict.remediation — green rows can never show an instruction
//     (the backend never attaches one, and the guard here is the second lock).
//  2. Purely additive markup in the name cell, appended AFTER the existing
//     chips, so sort/filter/expand behavior is untouched.
//  3. The "open" link stops propagation — it must navigate, not toggle the
//     row expand.
func TestFleetRemediationHintPinned(t *testing.T) {
	html := fleetStaticHTML(t)
	for _, want := range []string{
		"function remediationHTML(h, health)",
		`if (health !== "problem" && health !== "warning") return "";`,
		"(h.healthVerdict || {}).remediation",
		`class="rem-hint"`,
		`class="rem-link"`,
		// Appended after the existing chips in the name cell.
		"attachChipHTML(h) + remediationHTML(h, health)",
		// Row-expand isolation for the link.
		`tr.querySelector(".rem-link")`,
		// The style hooks the markup relies on.
		".rem-hint {",
		".rem-hint .rem-link {",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet static html missing %q", want)
		}
	}
}
