package spoke

import "github.com/hivecommons/hive/pkg/imageref"

const (
	ReleaseChannelStable    = "stable"
	ReleaseChannelCandidate = "candidate"
	ReleaseChannelEdge      = "edge"
)

func isReleaseChannel(tag string) bool {
	return imageref.IsReleaseChannel(tag)
}

// ResolveReleaseChannel is the spoke-side entry point for "what release channel
// does this spoke follow?" (#7092). It delegates to imageref so pkg/dashboard
// can answer without depending on pkg/hub. See imageref.ResolveSpokeChannel
// for the contract.
func ResolveReleaseChannel(imageRef, trackedChannel string) (channel string, resolved bool, tag string) {
	return imageref.ResolveSpokeChannel(imageRef, trackedChannel)
}
