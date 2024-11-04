package tailscale

import (
	"context"
	"net"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type DNSDialer struct {
	transport      *DNSTransport
	fallbackDialer N.Dialer
}

func (d *DNSDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if destination.IsFqdn() {
		panic("invalid request here")
	}
	for _, prefix := range d.transport.routePrefixes {
		if prefix.Contains(destination.Addr) {
			return d.transport.outbound.DialContext(ctx, network, destination)
		}
	}
	return d.fallbackDialer.DialContext(ctx, network, destination)
}

func (d *DNSDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if destination.IsFqdn() {
		panic("invalid request here")
	}
	for _, prefix := range d.transport.routePrefixes {
		if prefix.Contains(destination.Addr) {
			return d.transport.outbound.ListenPacket(ctx, destination)
		}
	}
	return d.fallbackDialer.ListenPacket(ctx, destination)
}
