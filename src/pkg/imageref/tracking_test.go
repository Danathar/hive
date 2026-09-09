package imageref

import (
	"strings"
	"testing"
)

func TestTracking(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want TrackingMode
	}{
		{name: "stable channel", ref: "ghcr.io/hivecommons/hive:stable", want: TrackingFloating},
		{name: "candidate channel", ref: "ghcr.io/hivecommons/hive:candidate", want: TrackingFloating},
		{name: "edge channel", ref: "ghcr.io/hivecommons/hive:edge", want: TrackingFloating},
		{name: "branch latest", ref: "ghcr.io/hivecommons/hive:v5-latest", want: TrackingFloating},
		{name: "sha tag", ref: "ghcr.io/hivecommons/hive:61c5ad7", want: TrackingPinned},
		{name: "version tag", ref: "ghcr.io/hivecommons/hive:v5.0.0", want: TrackingPinned},
		{name: "digest", ref: "ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64), want: TrackingPinned},
		{name: "tag and digest", ref: "ghcr.io/hivecommons/hive:stable@sha256:" + strings.Repeat("a", 64), want: TrackingPinned},
		{name: "sha512 digest", ref: "hive@sha512:" + strings.Repeat("a", 128), want: TrackingPinned},
		{name: "registry port floating", ref: "registry.internal:5000/hive:v5-latest", want: TrackingFloating},
		{name: "IPv6 registry", ref: "[2001:db8::1]:5000/hive:v5-latest", want: TrackingFloating},
		{name: "IPv6 registry without port", ref: "[::1]/hive:61c5ad7", want: TrackingPinned},
		{name: "invalid IPv6 registry", ref: "[bad::ip]/hive:stable", want: TrackingUnknown},
		{name: "unclosed IPv6 registry", ref: "[::1/hive:stable", want: TrackingUnknown},
		{name: "invalid IPv6 suffix", ref: "[::1]evil/hive:stable", want: TrackingUnknown},
		{name: "empty", ref: "", want: TrackingUnknown},
		{name: "untagged", ref: "ghcr.io/hivecommons/hive", want: TrackingUnknown},
		{name: "registry port only", ref: "registry.internal:5000/hive", want: TrackingUnknown},
		{name: "mangled missing tag separator", ref: "ghcr.io/hivecommons/hivev5-latest", want: TrackingUnknown},
		{name: "malformed tag", ref: "ghcr.io/hivecommons/hive:not a tag", want: TrackingUnknown},
		{name: "malformed digest", ref: "ghcr.io/hivecommons/hive@sha256", want: TrackingUnknown},
		{name: "multiple digest separators", ref: "ghcr.io/hive@sha256:abc@sha256:def", want: TrackingUnknown},
		{name: "url with credentials", ref: "https://user:secret@registry.example/hive:v5-latest", want: TrackingUnknown},
		{name: "truncated digest", ref: "hive@sha256:abc123", want: TrackingUnknown},
		{name: "invalid digest encoding", ref: "hive@sha256:" + strings.Repeat("z", 64), want: TrackingUnknown},
		{name: "unknown digest algorithm", ref: "hive@unknown:" + strings.Repeat("a", 64), want: TrackingUnknown},
		{name: "extra colon", ref: "hive::stable", want: TrackingUnknown},
		{name: "empty path segment", ref: "ghcr.io//hive:stable", want: TrackingUnknown},
		{name: "non-numeric registry port", ref: "registry:bad/hive:stable", want: TrackingUnknown},
		{name: "missing repository", ref: ":stable", want: TrackingUnknown},
		{name: "empty tag before digest", ref: "hive:@sha256:" + strings.Repeat("a", 64), want: TrackingUnknown},
		{name: "empty digest", ref: "hive@", want: TrackingUnknown},
		{name: "whitespace", ref: " hive:stable ", want: TrackingUnknown},
		{name: "overlong tag", ref: "hive:" + strings.Repeat("a", 129), want: TrackingUnknown},
		{name: "overlong name", ref: strings.Repeat("a", 256) + ":stable", want: TrackingUnknown},
		{name: "query string", ref: "hive:stable?token=secret", want: TrackingUnknown},
		{name: "legacy explicit latest semantics", ref: "hive:latest", want: TrackingPinned},
		{name: "legacy custom tag semantics", ref: "hive:custom", want: TrackingPinned},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Tracking(tt.ref); got != tt.want {
				t.Fatalf("Tracking(%q) = %q, want %q", tt.ref, got, tt.want)
			}
			if IsMutable(tt.ref) != (tt.want == TrackingFloating) || IsPinned(tt.ref) != (tt.want == TrackingPinned) {
				t.Fatal("upgrade/drift predicates disagree with tracking")
			}
			if tt.want == TrackingUnknown && ReleaseChannel(tt.ref) != "" {
				t.Fatal("malformed reference must not report a channel")
			}
		})
	}
}

func TestReleaseChannel(t *testing.T) {
	for _, tt := range []struct{ ref, want string }{
		{"hive:stable", "stable"},
		{"hive:candidate", "candidate"},
		{"hive:edge", "edge"},
		{"hive:stable@sha256:" + strings.Repeat("a", 64), "stable"},
		{"hive@sha256:" + strings.Repeat("a", 64), ""},
		{"hive:v5-latest", ""},
		{"hive:61c5ad7", ""},
		{"hive:stable@sha256:truncated", ""},
		{"bad//repo:stable", ""},
	} {
		if got := ReleaseChannel(tt.ref); got != tt.want {
			t.Errorf("ReleaseChannel(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
