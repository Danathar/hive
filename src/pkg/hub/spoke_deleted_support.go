package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/imageref"
)

const (
	ssoClockSkew     = 30 * time.Second
	infoTerminalKey  = "hive-terminal-v1"
	EnvTerminalKey   = "HIVE_TERMINAL_KEY"
	mutableTagSuffix = "-latest"
)

func imageTagIsMutable(image string) bool {
	return imageref.IsMutable(image)
}
