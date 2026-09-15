// Package imageref defines the shared interpretation of deployment image
// references used by upgrade, drift, and operator-facing provenance code.
package imageref

import (
	"net/netip"
	"regexp"
	"strings"
)

// TrackingMode describes whether an image reference follows a moving tag.
type TrackingMode string

const (
	TrackingUnknown  TrackingMode = "unknown"
	TrackingFloating TrackingMode = "floating"
	TrackingPinned   TrackingMode = "pinned"
)

const mutableTagSuffix = "-latest"

var (
	// Name validation is intentionally conservative. Kubernetes image fields
	// are OCI/Docker references, not URLs, so whitespace, credentials, query
	// strings, and fragments make the value unsuitable for display or a
	// provenance decision. Registry ports and an optional tag before a digest
	// remain valid.
	imagePathPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	registryPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(?::[0-9]+)?$`)
	imageTagPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^(?:sha256:[a-f0-9]{64}|sha384:[a-f0-9]{96}|sha512:[a-f0-9]{128})$`)
)

// Tracking classifies a Deployment-declared image reference.
//
// Release channels and per-branch "-latest" tags are floating. Digest refs
// and every other syntactically valid explicit tag are pinned. Empty,
// untagged, or malformed refs are unknown: lack of proof that a ref is mutable
// is not proof that it is an immutable pin.
// This preserves Hive's existing explicit-tag policy, including its treatment
// of bare :latest and custom tags as pinned; it does not query a registry or
// assert that an arbitrary registry enforces tag immutability.
func Tracking(ref string) TrackingMode {
	if ref == "" || strings.TrimSpace(ref) != ref {
		return TrackingUnknown
	}

	if strings.Contains(ref, "@") {
		if strings.Count(ref, "@") != 1 {
			return TrackingUnknown
		}
		parts := strings.SplitN(ref, "@", 2)
		name, tag := splitTag(parts[0])
		if !validImageName(name) || (name != parts[0] && !imageTagPattern.MatchString(tag)) || !digestPattern.MatchString(parts[1]) {
			return TrackingUnknown
		}
		return TrackingPinned
	}

	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash || !validImageName(ref[:colon]) {
		return TrackingUnknown
	}
	tag := ref[colon+1:]
	if !imageTagPattern.MatchString(tag) {
		return TrackingUnknown
	}
	if IsReleaseChannel(tag) || strings.HasSuffix(tag, mutableTagSuffix) {
		return TrackingFloating
	}
	return TrackingPinned
}

// IsMutable reports whether ref authoritatively names a moving image tag.
func IsMutable(ref string) bool { return Tracking(ref) == TrackingFloating }

// IsPinned reports whether ref authoritatively names an immutable tag/digest.
func IsPinned(ref string) bool { return Tracking(ref) == TrackingPinned }

// IsReleaseChannel reports whether tag is one of Hive's moving release tags.
func IsReleaseChannel(tag string) bool {
	switch tag {
	case "stable", "candidate", "edge":
		return true
	default:
		return false
	}
}

func validImageName(name string) bool {
	if len(name) > 255 {
		return false
	}
	if slash := strings.IndexByte(name, '/'); slash >= 0 {
		first := name[:slash]
		if strings.HasPrefix(first, "[") {
			close := strings.IndexByte(first, ']')
			if close < 0 {
				return false
			}
			addr, err := netip.ParseAddr(first[1:close])
			port := first[close+1:]
			if err != nil || !addr.Is6() || addr.Zone() != "" || (port != "" && (!strings.HasPrefix(port, ":") || !registryPattern.MatchString("registry"+port))) {
				return false
			}
			return imagePathPattern.MatchString(name[slash+1:])
		}
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			return registryPattern.MatchString(first) && imagePathPattern.MatchString(name[slash+1:])
		}
	}
	return imagePathPattern.MatchString(name)
}

func splitTag(ref string) (name, tag string) {
	if colon := strings.LastIndexByte(ref, ':'); colon > strings.LastIndexByte(ref, '/') {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// ReleaseChannel returns a channel only when it is explicit in a valid ref.
// A tag@digest ref can retain its channel label while still being pinned.
func ReleaseChannel(ref string) string {
	if Tracking(ref) == TrackingUnknown {
		return ""
	}
	name, _, _ := strings.Cut(ref, "@")
	_, tag := splitTag(name)
	if IsReleaseChannel(tag) {
		return tag
	}
	return ""
}

// ResolveSpokeChannel answers "what release channel does this spoke follow?"
// for the dashboard's release-visibility surface (#7092). It returns:
//   - channel  — the resolved release-channel name ("stable"/"candidate"/"edge"),
//     or "" when the spoke tracks a branch tag or a pin.
//   - resolved — true only when channel names a real release channel. false is
//     the signal the dashboard must render as "unknown / not a release channel"
//     rather than inventing a default.
//   - tag      — the image tag actually observed on the ref (before the channel
//     test), so the dashboard can show "unknown (image tag: v4-ab12cd)" even
//     when it is not a channel. "" when imageRef carries no tag at all.
//
// The reported image tag leads (it is what the kubelet pulls); trackedChannel
// is the fallback for spokes too old to report an image ref. It lives here, in
// a leaf package, so pkg/dashboard can use it without depending on pkg/hub.
func ResolveSpokeChannel(imageRef, trackedChannel string) (channel string, resolved bool, tag string) {
	tag = tagOf(sanitizeRef(imageRef))
	if IsReleaseChannel(tag) {
		return tag, true, tag
	}
	if imageRef == "" && IsReleaseChannel(trackedChannel) {
		return trackedChannel, true, tag
	}
	return "", false, tag
}

// sanitizeRef drops every rune outside the image-reference alphabet so a
// hostile or garbled ref cannot smuggle control characters into a rendered tag.
func sanitizeRef(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '@' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// tagOf returns the tag portion of ref, ignoring any digest suffix, or "" when
// the ref carries no tag.
func tagOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	_, tag := splitTag(ref)
	return tag
}
