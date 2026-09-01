package dashboard

import (
	"strings"
	"testing"
)

// contribute_hub_resolution_test.go pins the fix for kubestellar/hive#5092.
//
// THE DEFECT: `hive_hub := env("HIVE_HUB", "wss://hive.kubestellar.io/contribute")`
// is resolved by just at PARSE time, from the environment the `just` process
// itself was started with. A contributor's real hub does not live there — it
// arrives only when a recipe sources ~/.config/hive/contributor.env, which
// happens inside the recipe BODY, long after every {{hive_hub}} interpolation
// was already frozen to the fallback.
//
// Two recipes read that frozen value and were wrong in different ways:
//
//   - contribute-hive printed it as the "Hub:" banner line while the relay
//     connected somewhere else entirely. Confidently wrong, identically, on
//     every machine whose hub comes from the config file.
//   - contribute-status derived HUB_HTTP from it and then queried
//     /api/contributors/${CONTRIBUTOR_ID} — an ID that exists only on the
//     CONFIGURED hub. Measured against the default hub that request is a 404,
//     so "Could not fetch profile" was the guaranteed result for every
//     contributor on a hosted spoke.
//
// contribute-setup already had the right shape (`${_HUB:-{{hive_hub}}}`), which
// is why this reads as an oversight in two recipes rather than a design choice.
//
// These are source-level assertions because the failure is a shell-expansion
// ordering bug: there is no Go code path to exercise, and a runtime test would
// need a hub, a config file and a network. Pinning the ordering in the file is
// what stops it regressing.

// contributeHiveBannerLine returns the "Hub:" banner line from contribute-hive.
func contributeHiveBannerLine(t *testing.T, src string) string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `echo "Hub:`) {
			return trimmed
		}
	}
	t.Fatal(`the contribute-hive "Hub:" banner line was not found in the Justfile`)
	return ""
}

// contributeStatusRecipe returns the body of the contribute-status recipe, from
// its target line to the start of the next recipe.
func contributeStatusRecipe(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "\ncontribute-status:")
	if start < 0 {
		t.Fatal("the contribute-status recipe was not found in the Justfile")
	}
	rest := src[start+1:]
	end := strings.Index(rest, "\ncontribute-browse:")
	if end < 0 {
		t.Fatal("the end of the contribute-status recipe was not found in the Justfile")
	}
	return rest[:end]
}

// TestContributeHiveBannerReportsTheResolvedHub pins the banner fix. The banner
// exists to tell an operator which hub they are about to join; printing the
// parse-time default instead is worse than printing nothing, because it looks
// authoritative and is the same wrong answer everywhere.
func TestContributeHiveBannerReportsTheResolvedHub(t *testing.T) {
	line := contributeHiveBannerLine(t, justfileSource(t))

	// The sourced shell variable must be consulted first.
	if !strings.Contains(line, "${HIVE_HUB:-") {
		t.Errorf("the contribute-hive banner does not prefer the sourced ${HIVE_HUB}: %s", line)
	}
	// The parse-time value survives only as the fallback, never alone — an
	// operator with no configured hub should still see the built-in default.
	if !strings.Contains(line, "{{hive_hub}}") {
		t.Errorf("the contribute-hive banner dropped the {{hive_hub}} fallback: %s", line)
	}
}

// TestContributeStatusResolvesTheHubBeforeQueryingIt pins the ordering that was
// actually broken: the config file has to be sourced BEFORE any URL is derived
// from HIVE_HUB, or the queries go to the default hub carrying an ID that only
// exists on the configured one.
func TestContributeStatusResolvesTheHubBeforeQueryingIt(t *testing.T) {
	recipe := contributeStatusRecipe(t, justfileSource(t))

	sourceIdx := strings.Index(recipe, `source "{{config_dir}}/contributor.env"`)
	if sourceIdx < 0 {
		t.Fatal("contribute-status no longer sources contributor.env")
	}
	hubHTTPIdx := strings.Index(recipe, "HUB_HTTP=")
	if hubHTTPIdx < 0 {
		t.Fatal("contribute-status no longer derives HUB_HTTP")
	}
	if sourceIdx > hubHTTPIdx {
		t.Error("contribute-status derives HUB_HTTP before sourcing contributor.env, " +
			"so every query goes to the parse-time default hub (#5092)")
	}
	// The resolution itself must keep the default as a fallback rather than
	// requiring a config file to exist.
	if !strings.Contains(recipe, `HIVE_HUB="${HIVE_HUB:-{{hive_hub}}}"`) {
		t.Error("contribute-status does not resolve HIVE_HUB with the {{hive_hub}} fallback")
	}
}

// TestContributeStatusWalksEveryConfiguredHub covers the multi-hub case. A
// contributor may register with several hubs; contributor.env stores them as
// position-aligned comma-separated lists (hub[i] ↔ id[i]), the same convention
// the relay honors. Reporting only the first would quietly hide the others.
func TestContributeStatusWalksEveryConfiguredHub(t *testing.T) {
	recipe := contributeStatusRecipe(t, justfileSource(t))

	for _, want := range []string{
		`read -r -a _HUBS <<< "${HIVE_HUB}"`,
		`read -r -a _IDS <<< "${CONTRIBUTOR_ID}"`,
		`for i in "${!_HUBS[@]}"`,
	} {
		if !strings.Contains(recipe, want) {
			t.Errorf("contribute-status does not walk the configured hub list: missing %q", want)
		}
	}
	// The profile lookup must use the id paired with THIS hub, not a single
	// scalar CONTRIBUTOR_ID that would be wrong for every hub after the first.
	if !strings.Contains(recipe, `${_IDS[$i]}`) {
		t.Error("contribute-status does not pair each contributor id with its own hub")
	}
}
