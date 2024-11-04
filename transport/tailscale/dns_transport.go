package tailscale

import (
	"context"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/tailscale"
	"github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"

	mDNS "github.com/miekg/dns"
	"tailscale.com/ipn"
	nDNS "tailscale.com/net/dns"
	"tailscale.com/wgengine/wgcfg"
)

func init() {
	dns.RegisterTransport([]string{"tailscale"}, func(options dns.TransportOptions) (dns.Transport, error) {
		return NewDNSTransport(options)
	})
}

type DNSTransport struct {
	outboundTag      string
	options          dns.TransportOptions
	router           adapter.Router
	outbound         *tailscale.Tailscale
	rawConfig        *wgcfg.Config
	rawDNSConfig     **nDNS.Config
	rawRoutePrefixes *map[netip.Prefix]bool
	routePrefixes    []netip.Prefix
	dnsClient        *dns.Client
	routes           map[string][]dns.Transport
	hosts            map[string][]netip.Addr
}

func NewDNSTransport(options dns.TransportOptions) (dns.Transport, error) {
	linkURL, err := url.Parse(options.Address)
	if err != nil {
		return nil, err
	}
	if linkURL.Host == "" {
		return nil, E.New("missing tailscale outbound tag")
	}
	return &DNSTransport{
		outboundTag: linkURL.Host,
		options:     options,
		router:      adapter.RouterFromContext(options.Context),
	}, nil
}

func (t *DNSTransport) Name() string {
	return t.options.Name
}

func (t *DNSTransport) Start() error {
	rawOutbound, loaded := t.router.Outbound(t.outboundTag)
	if !loaded {
		return E.New("outbound not found: ", t.outboundTag)
	}
	tsOutbound, isTailscale := rawOutbound.(*tailscale.Tailscale)
	if !isTailscale {
		return E.New("outbound is not tailscale: ", t.outboundTag)
	}
	localBackend := tsOutbound.LocalBackend()
	engine := reflect.Indirect(reflect.Indirect(reflect.ValueOf(localBackend)).FieldByName("e").Elem())
	rawRoutePrefixes := engine.FieldByName("networkLogger").FieldByName("prefixes")
	rawConfig := engine.FieldByName("lastCfgFull")
	rawDNSConfig := engine.FieldByName("lastDNSConfig")
	t.outbound = tsOutbound
	t.rawRoutePrefixes = (*map[netip.Prefix]bool)(unsafe.Pointer(rawRoutePrefixes.UnsafeAddr()))
	t.rawConfig = (*wgcfg.Config)(unsafe.Pointer(rawConfig.UnsafeAddr()))
	t.rawDNSConfig = (**nDNS.Config)(unsafe.Pointer(rawDNSConfig.UnsafeAddr()))
	go localBackend.WatchNotifications(t.options.Context, ipn.NotifyInitialState, nil, func(roNotify *ipn.Notify) (keepGoing bool) {
		if roNotify.State != nil {
			if *roNotify.State == ipn.Running {
				err := t.updateDNSServers()
				if err == nil {
					t.options.Logger.Info("initialized")
				}
				return err != nil
			}
		}
		if roNotify.LoginFinished != nil {
			err := t.updateDNSServers()
			if err == nil {
				t.options.Logger.Info("initialized")
			}
			return err != nil
		}
		return true
	})
	return nil
}

func (t *DNSTransport) Reset() {
}

func (t *DNSTransport) updateDNSServers() error {
	dnsConfig := *t.rawDNSConfig
	if dnsConfig == nil {
		return os.ErrInvalid
	}
	rawRoutePrefixes := *t.rawRoutePrefixes
	if rawRoutePrefixes == nil {
		panic("nil route prefixes")
	}
	var routePrefixes []netip.Prefix
	for prefix := range rawRoutePrefixes {
		routePrefixes = append(routePrefixes, prefix)
	}
	t.routePrefixes = routePrefixes
	directDialerOnce := sync.OnceValue(func() N.Dialer {
		directDialer := common.Must1(dialer.NewDefault(t.router, option.DialerOptions{}))
		return &DNSDialer{transport: t, fallbackDialer: directDialer}
	})
	routes := make(map[string][]dns.Transport)
	for domain, resolvers := range dnsConfig.Routes {
		var myResolvers []dns.Transport
		for _, resolver := range resolvers {
			myDialer := directDialerOnce()
			if len(resolver.BootstrapResolution) > 0 {
				bootstrapTransport := common.Must1(dns.CreateTransport(dns.TransportOptions{
					Context: t.options.Context,
					Logger:  t.options.Logger,
					Dialer:  directDialerOnce(),
					Address: resolver.BootstrapResolution[0].String(),
				}))
				myDialer = dns.NewDialerWrapper(myDialer, t.dnsClient, bootstrapTransport, dns.DomainStrategyPreferIPv4, 0)
			}
			transport, err := dns.CreateTransport(dns.TransportOptions{
				Context: t.options.Context,
				Logger:  t.options.Logger,
				Dialer:  myDialer,
				Address: resolver.Addr,
			})
			if err != nil {
				return E.Cause(err, "parse resolver: ", resolver.Addr)
			}
			myResolvers = append(myResolvers, transport)
		}
		routes[domain.WithTrailingDot()] = myResolvers
	}
	hosts := make(map[string][]netip.Addr)
	for domain, addresses := range dnsConfig.Hosts {
		hosts[domain.WithTrailingDot()] = addresses
	}
	t.routes = routes
	t.hosts = hosts
	return nil
}

func (t *DNSTransport) Close() error {
	return nil
}

func (t *DNSTransport) Raw() bool {
	return true
}

func (t *DNSTransport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	if len(message.Question) != 1 {
		return nil, os.ErrInvalid
	}
	question := message.Question[0]
	addresses, hostsLoaded := t.hosts[question.Name]
	if hostsLoaded {
		switch question.Qtype {
		case mDNS.TypeA:
			addresses4 := common.Filter(addresses, func(addr netip.Addr) bool {
				return addr.Is4()
			})
			if len(addresses4) > 0 {
				return dns.FixedResponse(message.Id, question, addresses4, dns.DefaultTTL), nil
			}
		case mDNS.TypeAAAA:
			addresses6 := common.Filter(addresses, func(addr netip.Addr) bool {
				return addr.Is6()
			})
			if len(addresses6) > 0 {
				return dns.FixedResponse(message.Id, question, addresses6, dns.DefaultTTL), nil
			}
		}
	}
	for domainSuffix, transports := range t.routes {
		if strings.HasSuffix(question.Name, domainSuffix) {
			if len(transports) == 0 {
				return &mDNS.Msg{
					MsgHdr: mDNS.MsgHdr{
						Id:       message.Id,
						Rcode:    mDNS.RcodeNameError,
						Response: true,
					},
					Question: []mDNS.Question{question},
				}, nil
			}
			return transports[0].Exchange(ctx, message)
		}
	}
	return nil, dns.RCodeNameError
}

func (t *DNSTransport) Lookup(ctx context.Context, domain string, strategy dns.DomainStrategy) ([]netip.Addr, error) {
	return nil, os.ErrInvalid
}
