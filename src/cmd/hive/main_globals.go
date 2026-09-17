package main

import (
	"github.com/hivecommons/hive/pkg/appkey"
	"go.uber.org/automaxprocs/maxprocs"
)

// GitHub App private-key locations on a spoke live in pkg/appkey, which owns
// where they are, which one is correct for the App this hive claims, and how a
// hub-delivered key is written down (hivecommons/hive#5898 phase 1).
//
// A var, not a const, for the same reason the paths themselves used to be: a
// test points it at a temp dir and exercises the real resolution order.
// Production never reassigns it.
var appKeys = appkey.Default()

// init applies the container CPU quota to GOMAXPROCS with a silent logger. See
// the go.uber.org/automaxprocs/maxprocs import comment for why the banner the
// blank import would print is not acceptable in this binary. A failure here is
// deliberately ignored: it only means GOMAXPROCS keeps the Go default, which is
// the pre-existing behaviour and must never block startup.
func init() {
	_, _ = maxprocs.Set(maxprocs.Logger(func(string, ...interface{}) {}))
}
