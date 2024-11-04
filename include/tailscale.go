//go:build with_tailscale

package include

import (
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/protocol/tailscale"
	_ "github.com/sagernet/sing-box/transport/tailscale"
)

func registerTailscaleOutbound(registry *outbound.Registry) {
	tailscale.RegisterOutbound(registry)
}
