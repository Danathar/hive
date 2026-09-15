package spoke

import "testing"

// TestResolveSpokeReleaseChannelMin pins this package's minimal copy of the
// channel resolver (#7092). The copy exists because pkg/hub/spoke cannot import
// pkg/hub (import cycle), so the logic is duplicated rather than shared — which
// makes an independent test here mandatory: without one, the copy is free to
// drift from its pkg/hub twin while that twin's tests stay green.
//
// The table mirrors TestResolveSpokeReleaseChannel in pkg/hub exactly, and
// TestResolveSpokeReleaseChannelMatchesHubCopy in pkg/hub asserts the two
// implementations agree case-for-case.
func TestResolveSpokeReleaseChannelMin(t *testing.T) {
	cases := []struct {
		name         string
		imageRef     string
		tracked      string
		wantChannel  string
		wantResolved bool
		wantTag      string
	}{
		{"channel tag", "ghcr.io/hivecommons/hive:stable", "", "stable", true, "stable"},
		{"candidate tag", "ghcr.io/hivecommons/hive:candidate", "", "candidate", true, "candidate"},
		{"edge tag", "ghcr.io/hivecommons/hive:edge", "", "edge", true, "edge"},
		{"branch tag resolves unknown, tag preserved", "ghcr.io/hivecommons/hive:v4-latest", "", "", false, "v4-latest"},
		{"sha pin resolves unknown, tag preserved", "ghcr.io/hivecommons/hive:526ef71", "", "", false, "526ef71"},
		{"no image ref falls back to tracked channel", "", "candidate", "candidate", true, ""},
		{"no image ref, no channel resolves unknown", "", "", "", false, ""},
		// The reported image ref leads over intent: it is what the kubelet
		// pulls, and the two disagree exactly while a switch is on the wire.
		{"reported tag wins over intent, unknown with tag", "ghcr.io/hivecommons/hive:v4-latest", "stable", "", false, "v4-latest"},
		{"reported channel wins over a different intent", "ghcr.io/hivecommons/hive:stable", "edge", "stable", true, "stable"},
		// A tracked channel is only a fallback for a spoke too old to report an
		// image ref at all; a present-but-unparseable ref must not resurrect it.
		{"untagged ref does not fall back to intent", "ghcr.io/hivecommons/hive", "stable", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, resolved, tag := ResolveSpokeReleaseChannel(tc.imageRef, tc.tracked)
			if ch != tc.wantChannel || resolved != tc.wantResolved || tag != tc.wantTag {
				t.Errorf("ResolveSpokeReleaseChannel(%q, %q) = (%q, %v, %q), want (%q, %v, %q)",
					tc.imageRef, tc.tracked, ch, resolved, tag, tc.wantChannel, tc.wantResolved, tc.wantTag)
			}
			// resolved must never be true with an empty channel name: the
			// dashboard gates its "honest unknown" rendering on this flag, and
			// a true/"" pair would render a blank channel as authoritative.
			if resolved && ch == "" {
				t.Errorf("ResolveSpokeReleaseChannel(%q, %q) reported resolved with an empty channel",
					tc.imageRef, tc.tracked)
			}
		})
	}
}

// TestSanitizeImageRefMinStripsControlCharacters covers the sanitizer that
// guards the tag parse. An image ref arrives from a spoke heartbeat, so it is
// attacker-influenced input: anything outside the registry-legal alphabet is
// dropped rather than parsed.
func TestSanitizeImageRefMinStripsControlCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean ref is unchanged", "ghcr.io/hivecommons/hive:stable", "ghcr.io/hivecommons/hive:stable"},
		{"digest ref is unchanged", "ghcr.io/hivecommons/hive@sha256:abc123", "ghcr.io/hivecommons/hive@sha256:abc123"},
		{"whitespace and newlines are stripped", " ghcr.io/hivecommons/hive:stable\n", "ghcr.io/hivecommons/hive:stable"},
		{"shell metacharacters are stripped", "ghcr.io/hive:stable;rm -rf /", "ghcr.io/hive:stablerm-rf/"},
		{"quotes are stripped", "\"ghcr.io/hive:edge\"", "ghcr.io/hive:edge"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeImageRef(tc.in); got != tc.want {
				t.Errorf("sanitizeImageRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsReleaseChannelMin pins the channel allowlist. A branch tag must never
// be mistaken for a channel: channel resolution drives upgrade targeting, and a
// false positive would aim a spoke at a tag CI does not publish.
func TestIsReleaseChannelMin(t *testing.T) {
	for _, ch := range []string{ReleaseChannelStable, ReleaseChannelCandidate, ReleaseChannelEdge} {
		if !isReleaseChannel(ch) {
			t.Errorf("isReleaseChannel(%q) = false, want true", ch)
		}
	}
	for _, notCh := range []string{"", "v4", "v4-latest", "v5", "526ef71", "STABLE", "stable-1"} {
		if isReleaseChannel(notCh) {
			t.Errorf("isReleaseChannel(%q) = true, want false", notCh)
		}
	}
}
