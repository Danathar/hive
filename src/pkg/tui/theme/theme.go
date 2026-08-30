// Package theme is the TUI's palette: every color the frame draws, named by the ROLE
// it plays rather than by the color it happens to be, and given twice — once
// for a light terminal background and once for a dark one.
//
// WHY ADAPTIVE (T25, #5139). A single value cannot serve both backgrounds. The
// focus border shipped as ANSI 205 (#ff5fd7), chosen against a dark terminal
// where it is a ~7.9:1 contrast highlight that unmistakably says "this pane
// has focus". The same pink on a white background is ~2.4:1 — barely a tint —
// so an operator on a light terminal got a focus indicator they could not see.
// That is not a taste question, it is the one thing the border is for.
//
// WHY A PACKAGE rather than loose vars in tui. A token has exactly one
// definition and every call site names it, so the palette can be read in one
// place and a later change lands once. It is also the seam configurable themes
// (explicitly out of scope here) would plug into: swap the values here, not
// the call sites.
//
// SCOPE. The tokens below are the ones with real call sites today. The issue
// sketches header/status-glyph/muted-text tokens too, and they are
// deliberately absent: nothing renders a status glyph yet, and the header and
// footer carry Bold and Faint rather than a color. Defining tokens for them
// now would mean inventing colors for roles nothing draws — new visual design,
// which this task puts out of scope — and dead tokens are how a palette starts
// disagreeing with what is on screen. A pane task that needs one adds it here
// with its consumer, in the same change.
//
// WHERE THIS LIVES. A leaf package under pkg/tui, imported by both pkg/tui and
// pkg/tui/panes. It began life in package tui, on the reasoning that panes/
// rendered no color at all and so needed no access; the note there said the
// first pane that genuinely needed a token should move the file to a leaf
// package, "a rename and two import lines". The help overlay (T23, #5155) is
// that pane — it borders its box in the frame's emphasis color — and pkg/tui
// imports pkg/tui/panes, so reaching back up would be an import cycle. This is
// that move, made when a caller required it rather than in anticipation.
package theme

import "github.com/charmbracelet/lipgloss"

// The values are ANSI-256 indices, matching what
// the frame already used and what T25 was told not to expand on: the dark
// halves are exactly the colors that shipped, and each light half is its
// counterpart for a light background, not a new choice of hue.
//
// The pairs are matched by CONTRAST AGAINST THEIR OWN BACKGROUND, not by
// looking similar to each other, because that is what makes the frame read the
// same way in both:
//
//   - Border: 240 (#585858) is ~2.9:1 on black — present but recessive. The
//     same grey on white is ~6.8:1, which would make the unfocused borders
//     the loudest thing on screen. 245 (#8a8a8a) is ~3.5:1 on white, so the
//     border stays chrome rather than becoming content.
//   - BorderFocus: 205 (#ff5fd7) is ~7.9:1 on black. 127 (#af00af) is ~6.3:1
//     on white — the same magenta hue, darkened until it carries the same
//     emphasis on a light terminal that 205 carries on a dark one.
//
// Under termenv's Ascii profile — which is what `go test` renders through —
// both halves resolve to no color at all, so the golden files are unaffected
// by this change and by which background a machine reports.
var (
	// Border is the frame's de-emphasized chrome: the border around every
	// pane that does not have focus.
	Border = lipgloss.AdaptiveColor{Light: "245", Dark: "240"}

	// BorderFocus is the border around the focused pane, and the border of
	// the help overlay's box. It is the frame's only emphasis color, so it
	// must read as clearly emphasized on either background — see the
	// contrast note above.
	BorderFocus = lipgloss.AdaptiveColor{Light: "127", Dark: "205"}
)
