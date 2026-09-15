package imageref

import "testing"

func TestResolveSpokeChannel(t *testing.T) {
	cases := []struct {
		name, ref, tracked, wantChannel, wantTag string
		wantResolved                             bool
	}{
		{"channel tag leads", "ghcr.io/hivecommons/hive:stable", "edge", "stable", "stable", true},
		{"channel tag with digest", "ghcr.io/hivecommons/hive:edge@sha256:abc", "", "edge", "edge", true},
		{"branch tag is not a channel", "ghcr.io/hivecommons/hive:v4-ab12cd", "stable", "", "v4-ab12cd", false},
		{"digest pin has no tag", "ghcr.io/hivecommons/hive@sha256:abc", "stable", "", "", false},
		{"no ref falls back to tracked channel", "", "candidate", "candidate", "", true},
		{"no ref and bogus tracked", "", "v4", "", "", false},
		{"registry port is not a tag", "localhost:5000/hive", "", "", "", false},
		{"hostile runes stripped", "ghcr.io/hivecommons/hive:sta\x00ble", "", "stable", "stable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, ok, tag := ResolveSpokeChannel(tc.ref, tc.tracked)
			if ch != tc.wantChannel || ok != tc.wantResolved || tag != tc.wantTag {
				t.Fatalf("got (%q,%v,%q), want (%q,%v,%q)", ch, ok, tag, tc.wantChannel, tc.wantResolved, tc.wantTag)
			}
		})
	}
}
