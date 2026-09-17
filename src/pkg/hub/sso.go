package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func MintSSOToken(seedHex, username, role, hiveID string, now time.Time) string {
	return spoke.MintSSOToken(seedHex, username, role, hiveID, now)
}
