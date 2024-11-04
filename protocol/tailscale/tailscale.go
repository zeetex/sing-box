package tailscale

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox/platform"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/control"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/net/netmon"
	"tailscale.com/tsnet"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.TailscaleOutboundOptions](registry, C.TypeTailscale, NewTailscale)
}

type Tailscale struct {
	outbound.Adapter
	ctx               context.Context
	router            adapter.Router
	logger            logger.ContextLogger
	platformInterface platform.Interface
	server            *tsnet.Server
}

func NewTailscale(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.TailscaleOutboundOptions) (adapter.Outbound, error) {
	stateDirectory := options.StateDirectory
	if stateDirectory == "" {
		stateDirectory = "tailscale"
	}
	hostname := options.Hostname
	if hostname == "" {
		osHostname, _ := os.Hostname()
		osHostname = strings.TrimSpace(osHostname)
		hostname = osHostname
	}
	if hostname == "" {
		hostname = "sing-box"
	}
	stateDirectory = filemanager.BasePath(ctx, os.ExpandEnv(stateDirectory))
	stateDirectory, _ = filepath.Abs(stateDirectory)
	server := &tsnet.Server{
		Dir:      stateDirectory,
		Hostname: hostname,
		Logf: func(format string, args ...any) {
			logger.Trace(fmt.Sprintf(format, args...))
		},
		UserLogf: func(format string, args ...any) {
			logger.Debug(fmt.Sprintf(format, args...))
		},
		Ephemeral:  options.Ephemeral,
		AuthKey:    options.AuthKey,
		ControlURL: options.ControlURL,
	}
	return &Tailscale{
		Adapter:           outbound.NewAdapter(C.TypeTailscale, []string{N.NetworkTCP, N.NetworkUDP}, tag, nil),
		ctx:               ctx,
		router:            router,
		logger:            logger,
		platformInterface: service.FromContext[platform.Interface](ctx),
		server:            server,
	}, nil
}

func (t *Tailscale) Start() error {
	if t.platformInterface != nil {
		StateFileFunc = func() string {
			return filemanager.BasePath(t.ctx, "fake-state-file")
		}
		err := t.router.UpdateInterfaces()
		if err != nil {
			return err
		}
		netmon.RegisterInterfaceGetter(func() ([]netmon.Interface, error) {
			return common.Map(t.router.InterfaceFinder().Interfaces(), func(it control.Interface) netmon.Interface {
				return netmon.Interface{
					Interface: &net.Interface{
						Index:        it.Index,
						MTU:          it.MTU,
						Name:         it.Name,
						HardwareAddr: it.HardwareAddr,
						Flags:        it.Flags,
					},
					AltAddrs: common.Map(it.Addresses, func(it netip.Prefix) net.Addr {
						return &net.IPNet{
							IP:   it.Addr().AsSlice(),
							Mask: net.CIDRMask(it.Bits(), it.Addr().BitLen()),
						}
					}),
				}
			}), nil
		})
		if runtime.GOOS == "android" {
			setAndroidProtectFunc(t.platformInterface)
		}
	}
	err := t.server.Start()
	if err != nil {
		return err
	}
	go t.watchState()
	return nil
}

func (t *Tailscale) watchState() {
	localBackend := t.LocalBackend()
	localBackend.WatchNotifications(t.ctx, ipn.NotifyInitialState, nil, func(roNotify *ipn.Notify) (keepGoing bool) {
		if roNotify.State != nil && *roNotify.State != ipn.NeedsLogin && *roNotify.State != ipn.NoState {
			return false
		}
		authURL := localBackend.StatusWithoutPeers().AuthURL
		if authURL != "" {
			t.logger.Info("Waiting for authentication: ", authURL)
			if t.platformInterface != nil {
				err := t.platformInterface.SendNotification(&platform.Notification{
					Identifier: "tailscale-authentication",
					TypeName:   "Tailscale Authentication Notifications",
					TypeID:     10,
					Title:      "Tailscale Authentication",
					Body:       F.ToString("Tailscale outbound[", t.Tag(), "] is waiting for authentication."),
					OpenURL:    authURL,
				})
				if err != nil {
					t.logger.Error("send authentication notification: ", err)
				}
			}
			return false
		}
		return true
	})
}

func (t *Tailscale) Close() error {
	StateFileFunc = nil
	netmon.RegisterInterfaceGetter(nil)
	if runtime.GOOS == "android" {
		setAndroidProtectFunc(nil)
	}
	return common.Close(common.PtrOrNil(t.server))
}

func (t *Tailscale) LocalBackend() *ipnlocal.LocalBackend {
	rawLocalBackend := reflect.Indirect(reflect.ValueOf(t.server)).FieldByName("lb").UnsafePointer()
	return (*ipnlocal.LocalBackend)(rawLocalBackend)
}

func (t *Tailscale) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch network {
	case N.NetworkTCP:
		t.logger.InfoContext(ctx, "outbound connection to ", destination)
	case N.NetworkUDP:
		t.logger.InfoContext(ctx, "outbound packet connection to ", destination)
	}
	if destination.IsFqdn() {
		destinationAddresses, err := t.router.LookupDefault(ctx, destination.Fqdn)
		if err != nil {
			return nil, err
		}
		return N.DialSerial(ctx, t, network, destination, destinationAddresses)
	}
	return t.server.Dial(ctx, network, destination.String())
}

func (t *Tailscale) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	t.logger.InfoContext(ctx, "outbound packet connection to ", destination)
	if destination.IsFqdn() {
		destinationAddresses, err := t.router.LookupDefault(ctx, destination.Fqdn)
		if err != nil {
			return nil, err
		}
		packetConn, _, err := N.ListenSerial(ctx, t, destination, destinationAddresses)
		if err != nil {
			return nil, err
		}
		return packetConn, err
	}
	return t.server.ListenPacket("udp", "")
}

func (t *Tailscale) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return outbound.NewDirectConnection(ctx, t.router, t, conn, metadata, dns.DomainStrategyAsIS)
}

func (t *Tailscale) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	return outbound.NewDirectPacketConnection(ctx, t.router, t, conn, metadata, dns.DomainStrategyAsIS)
}
