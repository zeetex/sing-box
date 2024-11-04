package tailscale

import (
	_ "unsafe"

	C "github.com/sagernet/sing-box/constant"

	"tailscale.com/types/lazy"
)

//go:linkname StateFileFunc tailscale.com/paths.stateFileFunc
var StateFileFunc func() string

//go:linkname versionShort tailscale.com/version.short
var versionShort lazy.SyncValue[string]

//go:linkname versionLong tailscale.com/version.long
var versionLong lazy.SyncValue[string]

//go:linkname debugDisablePortlist tailscale.com/portlist.debugDisablePortlist
var debugDisablePortlist func() bool

func init() {
	versionShort.Get(func() string {
		return "sing-box " + C.Version
	})
	versionLong.Get(func() string {
		return "sing-box " + C.Version
	})
	debugDisablePortlist = func() bool {
		return true
	}
}
