package spoke

import "github.com/hivecommons/hive/pkg/imageref"

const (
	ReleaseChannelStable    = "stable"
	ReleaseChannelCandidate = "candidate"
	ReleaseChannelEdge      = "edge"
)

var releaseChannels = []string{ReleaseChannelStable, ReleaseChannelCandidate, ReleaseChannelEdge}

func isReleaseChannel(tag string) bool {
	return imageref.IsReleaseChannel(tag)
}
