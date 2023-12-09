package executor

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/adapter/outboundgroup"
	"github.com/metacubex/mihomo/component/auth"
	"github.com/metacubex/mihomo/component/ca"
	"github.com/metacubex/mihomo/component/dialer"
	G "github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/iface"
	"github.com/metacubex/mihomo/component/profile"
	"github.com/metacubex/mihomo/component/profile/cachefile"
	"github.com/metacubex/mihomo/component/resolver"
	SNI "github.com/metacubex/mihomo/component/sniffer"
	"github.com/metacubex/mihomo/component/trie"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/constant/features"
	"github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/dns"
	"github.com/metacubex/mihomo/listener"
	authStore "github.com/metacubex/mihomo/listener/auth"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/listener/inner"
	"github.com/metacubex/mihomo/listener/tproxy"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/ntp"
	"github.com/metacubex/mihomo/tunnel"
)

var mux sync.Mutex

func readConfig(path string) ([]byte, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("configuration file %s is empty", path)
	}

	return data, err
}

// Parse config with default config path
func Parse() (*config.Config, error) {
	return ParseWithPath(C.Path.Config())
}

// ParseWithPath parse config with custom config path
func ParseWithPath(path string) (*config.Config, error) {
	buf, err := readConfig(path)
	if err != nil {
		return nil, err
	}

	return ParseWithBytes(buf)
}

// ParseWithBytes config with buffer
func ParseWithBytes(buf []byte) (*config.Config, error) {
	return config.Parse(buf)
}

// ApplyConfig dispatch configure to all parts
func ApplyConfig(cfg *config.Config, force bool) {
	mux.Lock()
	defer mux.Unlock()

	tunnel.OnSuspend()

	ca.ResetCertificate()
	for _, c := range cfg.TLS.CustomTrustCert {
		if err := ca.AddCertificate(c); err != nil {
			log.Warnln("%s\nadd error: %s", c, err.Error())
		}
	}

	updateUsers(cfg.Users)
	updateProxies(cfg.Proxies, cfg.Providers)
	updateRules(cfg.Rules, cfg.SubRules, cfg.RuleProviders)
	updateSniffer(cfg.Sniffer)
	updateHosts(cfg.Hosts)
	updateGeneral(cfg.General)
	updateNTP(cfg.NTP)
	updateDNS(cfg.DNS, cfg.RuleProviders, cfg.General.IPv6)
	updateListeners(cfg.General, cfg.Listeners, force)
	updateIPTables(cfg)
	updateTun(cfg.General)
	updateExperimental(cfg)
	updateTunnels(cfg.Tunnels)

	tunnel.OnInnerLoading()

	initInnerTcp()
	loadProxyProvider(cfg.Providers)
	updateProfile(cfg)
	loadRuleProvider(cfg.RuleProviders)
	runtime.GC()
	tunnel.OnRunning()
	hcCompatibleProvider(cfg.Providers)

	log.SetLevel(cfg.General.LogLevel)
}

func initInnerTcp() {
	inner.New(tunnel.Tunnel)
}

func GetGeneral() *config.General {
	ports := listener.GetPorts()
	var authenticator []string
	if auth := authStore.Authenticator(); auth != nil {
		authenticator = auth.Users()
	}

	general := &config.General{
		Inbound: config.Inbound{
			Port:              ports.Port,
			SocksPort:         ports.SocksPort,
			RedirPort:         ports.RedirPort,
			TProxyPort:        ports.TProxyPort,
			MixedPort:         ports.MixedPort,
			Tun:               listener.GetTunConf(),
			TuicServer:        listener.GetTuicConf(),
			ShadowSocksConfig: ports.ShadowSocksConfig,
			VmessConfig:       ports.VmessConfig,
			Authentication:    authenticator,
			SkipAuthPrefixes:  inbound.SkipAuthPrefixes(),
			AllowLan:          listener.AllowLan(),
			BindAddress:       listener.BindAddress(),
		},
		Controller:    config.Controller{},
		Mode:          tunnel.Mode(),
		LogLevel:      log.Level(),
		IPv6:          !resolver.DisableIPv6,
		GeodataLoader: G.LoaderName(),
		Interface:     dialer.DefaultInterface.Load(),
		Sniffing:      tunnel.IsSniffing(),
		TCPConcurrent: dialer.GetTcpConcurrent(),
	}

	return general
}

func updateListeners(general *config.General, listeners map[string]C.InboundListener, force bool) {
	listener.PatchInboundListeners(listeners, tunnel.Tunnel, true)
	if !force {
		return
	}

	allowLan := general.AllowLan
	listener.SetAllowLan(allowLan)
	inbound.SetSkipAuthPrefixes(general.SkipAuthPrefixes)

	bindAddress := general.BindAddress
	listener.SetBindAddress(bindAddress)
	listener.ReCreateHTTP(general.Port, tunnel.Tunnel)
	listener.ReCreateSocks(general.SocksPort, tunnel.Tunnel)
	listener.ReCreateRedir(general.RedirPort, tunnel.Tunnel)
	if !features.CMFA {
		listener.ReCreateAutoRedir(general.EBpf.AutoRedir, tunnel.Tunnel)
	}
	listener.ReCreateTProxy(general.TProxyPort, tunnel.Tunnel)
	listener.ReCreateMixed(general.MixedPort, tunnel.Tunnel)
	listener.ReCreateShadowSocks(general.ShadowSocksConfig, tunnel.Tunnel)
	listener.ReCreateVmess(general.VmessConfig, tunnel.Tunnel)
	listener.ReCreateTuic(general.TuicServer, tunnel.Tunnel)
}

func updateExperimental(c *config.Config) {
	if c.Experimental.QUICGoDisableGSO {
		_ = os.Setenv("QUIC_GO_DISABLE_GSO", strconv.FormatBool(true))
	}
	if c.Experimental.QUICGoDisableECN {
		_ = os.Setenv("QUIC_GO_DISABLE_ECN", strconv.FormatBool(true))
	}
}

func updateNTP(c *config.NTP) {
	if c.Enable {
		ntp.ReCreateNTPService(
			net.JoinHostPort(c.Server, strconv.Itoa(c.Port)),
			time.Duration(c.Interval),
			c.DialerProxy,
			c.WriteToSystem,
		)
	}
}

func updateDNS(c *config.DNS, ruleProvider map[string]provider.RuleProvider, generalIPv6 bool) {
	if !c.Enable {
		resolver.DefaultResolver = nil
		resolver.DefaultHostMapper = nil
		resolver.DefaultLocalServer = nil
		dns.ReCreateServer("", nil, nil)
		return
	}
	cfg := dns.Config{
		Main:         c.NameServer,
		Fallback:     c.Fallback,
		IPv6:         c.IPv6 && generalIPv6,
		IPv6Timeout:  c.IPv6Timeout,
		EnhancedMode: c.EnhancedMode,
		Pool:         c.FakeIPRange,
		Hosts:        c.Hosts,
		FallbackFilter: dns.FallbackFilter{
			GeoIP:     c.FallbackFilter.GeoIP,
			GeoIPCode: c.FallbackFilter.GeoIPCode,
			IPCIDR:    c.FallbackFilter.IPCIDR,
			Domain:    c.FallbackFilter.Domain,
			GeoSite:   c.FallbackFilter.GeoSite,
		},
		Default:        c.DefaultNameserver,
		Policy:         c.NameServerPolicy,
		ProxyServer:    c.ProxyServerNameserver,
		RuleProviders:  ruleProvider,
		CacheAlgorithm: c.CacheAlgorithm,
	}

	r := dns.NewResolver(cfg)
	pr := dns.NewProxyServerHostResolver(r)
	m := dns.NewEnhancer(cfg)

	// reuse cache of old host mapper
	if old := resolver.DefaultHostMapper; old != nil {
		m.PatchFrom(old.(*dns.ResolverEnhancer))
	}

	resolver.DefaultResolver = r
	resolver.DefaultHostMapper = m
	resolver.DefaultLocalServer = dns.NewLocalServer(r, m)

	if pr.Invalid() {
		resolver.ProxyServerHostResolver = pr
	}

	dns.ReCreateServer(c.Listen, r, m)
}

func updateHosts(tree *trie.DomainTrie[resolver.HostValue]) {
	resolver.DefaultHosts = resolver.NewHosts(tree)
}

func updateProxies(proxies map[string]C.Proxy, providers map[string]provider.ProxyProvider) {
	tunnel.UpdateProxies(proxies, providers)
}

func updateRules(rules []C.Rule, subRules map[string][]C.Rule, ruleProviders map[string]provider.RuleProvider) {
	tunnel.UpdateRules(rules, subRules, ruleProviders)
}

func loadProvider(pv provider.Provider) {
	if pv.VehicleType() == provider.Compatible {
		return
	} else {
		log.Infoln("Start initial provider %s", (pv).Name())
	}

	if err := pv.Initial(); err != nil {
		switch pv.Type() {
		case provider.Proxy:
			{
				log.Errorln("initial proxy provider %s error: %v", (pv).Name(), err)
			}
		case provider.Rule:
			{
				log.Errorln("initial rule provider %s error: %v", (pv).Name(), err)
			}

		}
	}
}

func loadRuleProvider(ruleProviders map[string]provider.RuleProvider) {
	wg := sync.WaitGroup{}
	ch := make(chan struct{}, concurrentCount)
	for _, ruleProvider := range ruleProviders {
		ruleProvider := ruleProvider
		wg.Add(1)
		ch <- struct{}{}
		go func() {
			defer func() { <-ch; wg.Done() }()
			loadProvider(ruleProvider)

		}()
	}

	wg.Wait()
}

func loadProxyProvider(proxyProviders map[string]provider.ProxyProvider) {
	// limit concurrent size
	wg := sync.WaitGroup{}
	ch := make(chan struct{}, concurrentCount)
	for _, proxyProvider := range proxyProviders {
		proxyProvider := proxyProvider
		wg.Add(1)
		ch <- struct{}{}
		go func() {
			defer func() { <-ch; wg.Done() }()
			loadProvider(proxyProvider)
		}()
	}

	wg.Wait()
}
func hcCompatibleProvider(proxyProviders map[string]provider.ProxyProvider) {
	// limit concurrent size
	wg := sync.WaitGroup{}
	ch := make(chan struct{}, concurrentCount)
	for _, proxyProvider := range proxyProviders {
		proxyProvider := proxyProvider
		if proxyProvider.VehicleType() == provider.Compatible {
			log.Infoln("Start initial Compatible provider %s", proxyProvider.Name())
			wg.Add(1)
			ch <- struct{}{}
			go func() {
				defer func() { <-ch; wg.Done() }()
				if err := proxyProvider.Initial(); err != nil {
					log.Errorln("initial Compatible provider %s error: %v", proxyProvider.Name(), err)
				}
			}()
		}

	}

}
func updateTun(general *config.General) {
	if general == nil {
		return
	}
	listener.ReCreateTun(general.Tun, tunnel.Tunnel)
	listener.ReCreateRedirToTun(general.Tun.RedirectToTun)
}

func updateSniffer(sniffer *config.Sniffer) {
	if sniffer.Enable {
		dispatcher, err := SNI.NewSnifferDispatcher(
			sniffer.Sniffers, sniffer.ForceDomain, sniffer.SkipDomain,
			sniffer.ForceDnsMapping, sniffer.ParsePureIp,
		)
		if err != nil {
			log.Warnln("initial sniffer failed, err:%v", err)
		}

		tunnel.UpdateSniffer(dispatcher)
		log.Infoln("Sniffer is loaded and working")
	} else {
		dispatcher, err := SNI.NewCloseSnifferDispatcher()
		if err != nil {
			log.Warnln("initial sniffer failed, err:%v", err)
		}

		tunnel.UpdateSniffer(dispatcher)
		log.Infoln("Sniffer is closed")
	}
}

func updateTunnels(tunnels []LC.Tunnel) {
	listener.PatchTunnel(tunnels, tunnel.Tunnel)
}

func updateGeneral(general *config.General) {
	tunnel.SetMode(general.Mode)
	tunnel.SetFindProcessMode(general.FindProcessMode)
	resolver.DisableIPv6 = !general.IPv6

	if general.TCPConcurrent {
		dialer.SetTcpConcurrent(general.TCPConcurrent)
		log.Infoln("Use tcp concurrent")
	}

	inbound.SetTfo(general.InboundTfo)
	inbound.SetMPTCP(general.InboundMPTCP)

	adapter.UnifiedDelay.Store(general.UnifiedDelay)

	dialer.DefaultInterface.Store(general.Interface)
	dialer.DefaultRoutingMark.Store(int32(general.RoutingMark))
	if general.RoutingMark > 0 {
		log.Infoln("Use routing mark: %#x", general.RoutingMark)
	}

	iface.FlushCache()
	geodataLoader := general.GeodataLoader
	G.SetLoader(geodataLoader)
}

func updateUsers(users []auth.AuthUser) {
	authenticator := auth.NewAuthenticator(users)
	authStore.SetAuthenticator(authenticator)
	if authenticator != nil {
		log.Infoln("Authentication of local server updated")
	}
}

func updateProfile(cfg *config.Config) {
	profileCfg := cfg.Profile

	profile.StoreSelected.Store(profileCfg.StoreSelected)
	if profileCfg.StoreSelected {
		patchSelectGroup(cfg.Proxies)
	}
}

func patchSelectGroup(proxies map[string]C.Proxy) {
	mapping := cachefile.Cache().SelectedMap()
	if mapping == nil {
		return
	}

	for name, proxy := range proxies {
		outbound, ok := proxy.(*adapter.Proxy)
		if !ok {
			continue
		}

		selector, ok := outbound.ProxyAdapter.(outboundgroup.SelectAble)
		if !ok {
			continue
		}

		selected, exist := mapping[name]
		if !exist {
			continue
		}

		selector.ForceSet(selected)
	}
}

func updateIPTables(cfg *config.Config) {
	tproxy.CleanupTProxyIPTables()

	iptables := cfg.IPTables
	if runtime.GOOS != "linux" || !iptables.Enable {
		return
	}

	var err error
	defer func() {
		if err != nil {
			log.Errorln("[IPTABLES] setting iptables failed: %s", err.Error())
			os.Exit(2)
		}
	}()

	if cfg.General.Tun.Enable {
		err = fmt.Errorf("when tun is enabled, iptables cannot be set automatically")
		return
	}

	var (
		inboundInterface = "lo"
		bypass           = iptables.Bypass
		tProxyPort       = cfg.General.TProxyPort
		dnsCfg           = cfg.DNS
	)

	if tProxyPort == 0 {
		err = fmt.Errorf("tproxy-port must be greater than zero")
		return
	}

	if !dnsCfg.Enable {
		err = fmt.Errorf("DNS server must be enable")
		return
	}

	dnsPort, err := netip.ParseAddrPort(dnsCfg.Listen)
	if err != nil {
		err = fmt.Errorf("DNS server must be correct")
		return
	}

	if iptables.InboundInterface != "" {
		inboundInterface = iptables.InboundInterface
	}

	if dialer.DefaultRoutingMark.Load() == 0 {
		dialer.DefaultRoutingMark.Store(2158)
	}

	err = tproxy.SetTProxyIPTables(inboundInterface, bypass, uint16(tProxyPort), dnsPort.Port())
	if err != nil {
		return
	}

	log.Infoln("[IPTABLES] Setting iptables completed")
}

func Shutdown() {
	listener.Cleanup()
	tproxy.CleanupTProxyIPTables()
	resolver.StoreFakePoolState()

	log.Warnln("Mihomo shutting down")
}
