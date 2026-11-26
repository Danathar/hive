package delegatedecision

import (
	"strings"
	"testing"
)

// A fork PR parked for a fork guard (stage reason `fork-guard:<checks>`,
// hivecommons/hive#14273): its only red check refuses the PR because the head
// is in a fork. Neither the contributor nor an in-place adoption can turn that
// check green (both push to the same fork branch), so a delegated decision
// must not hand anyone "make CI green" or unpark the PR to do it.
//
// projectbluefin/utah-packages#1309, 2026-11-25 8:13 PM EST: parked
// `fork-guard:Canary`, adopted in place; "Let hive decide" answered
// "hive's fixer: make CI green without widening scope on #1309; then it
// merges" and the PR went back to 085 seconds later.
func TestDecideForkGuardParkDoesNotAskForGreenCI(t *testing.T) {
	const refused = "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	base := Input{Kind: KindPR, Repo: "acme/widget", Number: 1309, Author: "dtg01100", ContributorOnline: true,
		AdoptedByHive: true, Reasons: []string{"fork-guard:Canary"}}
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{name: "adopted in place (utah-packages#1309)", in: base},
		{name: "adopted, push refused, author online", in: with(base, func(i *Input) { i.AdoptedPushRefused = refused })},
		{name: "adopted, push refused, author offline", in: with(base, func(i *Input) {
			i.AdoptedPushRefused = refused
			i.ContributorOnline = false
		})},
		{name: "not adopted, author online", in: with(base, func(i *Input) { i.AdoptedByHive = false })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if strings.Contains(d.Text, "make CI green") {
				t.Errorf("a fork-guard park was told to make CI green: rule %s verdict %s text %q", d.Rule, d.Verdict, d.Text)
			}
			if d.Verdict == VerdictDirection {
				t.Errorf("a fork-guard park was unparked to a fixer or author who cannot turn the guarded check green: rule %s text %q", d.Rule, d.Text)
			}
		})
	}
}
