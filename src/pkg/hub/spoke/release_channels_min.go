package spoke

import (
	"strings"

	"github.com/hivecommons/hive/pkg/imageref"
)

const (
	ReleaseChannelStable    = "stable"
	ReleaseChannelCandidate = "candidate"
	ReleaseChannelEdge      = "edge"
)

func isReleaseChannel(tag string) bool {
	return imageref.IsReleaseChannel(tag)
}

// ResolveSpokeReleaseChannel resolves the release channel a spoke's Deployment
// actually follows and reports how it resolved, so the spoke dashboard can be
// honest when its image tag is not a channel.
func ResolveSpokeReleaseChannel(imageRef, trackedChannel string) (channel string, resolved bool, tag string) {
	tag = imageTagOf(sanitizeImageRef(imageRef))
	if isReleaseChannel(tag) {
		return tag, true, tag
	}
	if imageRef == "" && isReleaseChannel(trackedChannel) {
		return trackedChannel, true, tag
	}
	return "", false, tag
}

func sanitizeImageRef(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '@' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
