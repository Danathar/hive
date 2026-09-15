package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestResolveSpokeReleaseChannelMatchesHubCopy is a drift guard over a
// deliberate duplicate.
//
// pkg/hub/spoke cannot import pkg/hub (import cycle), so the channel resolver
// exists twice: the authoritative copy here in channel_targeting.go, and the
// minimal copy in pkg/hub/spoke/release_channels_min.go. Duplication is the
// accepted cost of breaking the cycle — silent divergence is not. The two
// implementations decide what a spoke is actually running, and that answer
// drives upgrade targeting; if they disagree, the hub aims at one channel while
// the spoke's own dashboard reports another, which is exactly the class of
// "lost in version space" confusion #7092 reported.
//
// This test is the only thing that can catch that divergence, because each
// copy's own table test passes independently while they drift apart. Every case
// asserts all three return values, so a change to one copy's resolution order,
// its fallback rule, or its reported tag fails here.
func TestResolveSpokeReleaseChannelMatchesHubCopy(t *testing.T) {
	cases := []struct {
		name     string
		imageRef string
		tracked  string
	}{
		{"channel tag", "ghcr.io/hivecommons/hive:stable", ""},
		{"candidate tag", "ghcr.io/hivecommons/hive:candidate", ""},
		{"edge tag", "ghcr.io/hivecommons/hive:edge", ""},
		{"branch tag", "ghcr.io/hivecommons/hive:v4-latest", ""},
		{"sha pin", "ghcr.io/hivecommons/hive:526ef71", ""},
		{"digest pin", "ghcr.io/hivecommons/hive@sha256:abc123", ""},
		{"no ref falls back to intent", "", "candidate"},
		{"no ref, no intent", "", ""},
		{"reported tag wins over intent", "ghcr.io/hivecommons/hive:v4-latest", "stable"},
		{"reported channel wins over different intent", "ghcr.io/hivecommons/hive:stable", "edge"},
		{"untagged ref with intent", "ghcr.io/hivecommons/hive", "stable"},
		{"unsanitized ref", " ghcr.io/hivecommons/hive:stable\n", ""},
		{"intent that is not a channel", "", "v4-latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hubCh, hubResolved, hubTag := ResolveSpokeReleaseChannel(tc.imageRef, tc.tracked)
			spokeCh, spokeResolved, spokeTag := spoke.ResolveSpokeReleaseChannel(tc.imageRef, tc.tracked)
			if hubCh != spokeCh || hubResolved != spokeResolved || hubTag != spokeTag {
				t.Errorf("copies disagree for (imageRef=%q, tracked=%q):\n  pkg/hub       = (%q, %v, %q)\n  pkg/hub/spoke = (%q, %v, %q)",
					tc.imageRef, tc.tracked,
					hubCh, hubResolved, hubTag,
					spokeCh, spokeResolved, spokeTag)
			}
		})
	}
}
