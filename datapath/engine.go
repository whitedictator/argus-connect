//go:build android || linux

// Package datapath terminates IP flows received from Android's tethering test
// network and originates equivalent sockets on a selected Android VPN Network.
package datapath

import (
	"fmt"
	"net"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const bridgeNIC tcpip.NICID = 1

type Engine struct {
	networkHandle atomic.Int64
	network       *networkSelector
	stack         *stack.Stack
	policy        *lightPolicy
}

func Start(tunFD int, mtu int) (*Engine, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocolCUBIC,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
	})
	link, err := fdbased.New(&fdbased.Options{FDs: []int{tunFD}, MTU: uint32(mtu)})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("open TUN fd %d: %w", tunFD, err)
	}
	if failure := s.CreateNIC(bridgeNIC, link); failure != nil {
		s.Close()
		return nil, fmt.Errorf("create bridge NIC: %v", failure)
	}
	s.SetPromiscuousMode(bridgeNIC, true)
	s.SetSpoofing(bridgeNIC, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: bridgeNIC},
		{Destination: header.IPv6EmptySubnet, NIC: bridgeNIC},
	})

	selector := &networkSelector{}
	dialer := &net.Dialer{Timeout: connectTimeout, Control: selector.control}
	policy := newLightPolicy(dialer)
	attachTCP(s, dialer, policy)
	attachUDP(s, dialer, policy)
	return &Engine{network: selector, stack: s, policy: policy}, nil
}

// SetLightMode enables the local anti-interference policy. It never changes the
// device's public exit: upstream sockets stay on the selected physical Network.
func (e *Engine) SetLightMode(enabled bool) {
	if e.policy != nil {
		e.policy.enabled.Store(enabled)
	}
}

// LightStatus returns a compact, content-free health summary for the Android UI.
func (e *Engine) LightStatus() string {
	if e.policy == nil {
		return `{"enabled":false}`
	}
	return e.policy.statusJSON()
}

// ProbeServiceRoute checks only fixed public service endpoints, on the selected
// physical network, and retains an exact-host route only after verified HTTPS.
func (e *Engine) ProbeServiceRoute(host, caPEM string) string {
	return e.policy.probeServiceRoute(normalizeProbeHost(host), caPEM)
}

// LoadLightState restores only aggregate strategy outcomes. Domain names,
// addresses and traffic metadata are deliberately never persisted.
func (e *Engine) LoadLightState(encoded string) {
	if e.policy != nil {
		e.policy.loadState(encoded)
	}
}

func (e *Engine) StrategyMemory() string {
	if e.policy == nil {
		return ""
	}
	return e.policy.strategyMemoryJSON()
}

// SetProxyPool configures optional SOCKS5 fallbacks. Entries are tried only
// after direct service routes fail; credentials and endpoints are never added
// to LightStatus or persisted by the native engine.
func (e *Engine) SetProxyPool(encoded string) {
	if e.policy != nil {
		e.policy.proxies.configure(encoded)
		e.policy.mtproxies.configure(encoded, false)
	}
}

// SetSignalRelay opts into the explicitly selected Signal-only public relay.
func (e *Engine) SetSignalRelay(enabled bool) {
	if e.policy != nil {
		e.policy.signalRelay.Store(enabled)
	}
}

// SetVoiceMappingTrial is an ephemeral diagnostic switch, off on every start.
// It changes only new voice/STUN UDP flows, never TCP, saved tactics or proxies.
func (e *Engine) SetVoiceMappingTrial(enabled bool) {
	if e.policy != nil {
		e.policy.voiceMappingTrial.Store(enabled)
		if !enabled {
			e.policy.udpMappings.reset(false)
		}
	}
}

// SetSignalDirectTrial controls an ephemeral, exact-host diagnostic route.
// Defaults off and is never persisted or applied to any other service.
func (e *Engine) SetSignalDirectTrial(enabled bool) {
	if e.policy != nil {
		e.policy.signalDirectTrial.Store(enabled)
	}
}

// SetMTProxyDiscovery opts into fetching and probing public Telegram proxies.
// Off by default. It never changes Telegram settings or other service routes.
func (e *Engine) SetMTProxyDiscovery(enabled bool) {
	if e.policy != nil {
		e.policy.mtproxies.mu.Lock()
		e.policy.mtproxies.auto = enabled
		e.policy.mtproxies.mu.Unlock()
	}
}

// SetNAT64Prefix enables safe IPv4-to-IPv6 synthesis for the prefix Android
// discovered on the selected carrier network. Empty or non-/96 input disables
// synthesis.
func (e *Engine) SetNAT64Prefix(prefix string) {
	if e.policy != nil {
		e.policy.setNAT64Prefix(prefix)
	}
}

// SetNetwork makes every future upstream socket use this Android Network.
// A zero handle deliberately rejects new traffic instead of using the default.
func (e *Engine) SetNetwork(handle int64) {
	previous := e.networkHandle.Swap(handle)
	if e.network != nil {
		e.network.set(uint64(handle))
	}
	// Update selection first, then close old mappings under their mutex so a
	// concurrently opening socket cannot escape on the old physical Network.
	if previous != handle && e.policy != nil {
		e.policy.udpMappings.reset(false)
	}
}

// VerifyBinding asks the kernel to associate a fresh socket with the selected
// Android Network. It proves the handle is live before the hotspot is exposed.
func (e *Engine) VerifyBinding() error {
	if e.network == nil {
		return fmt.Errorf("network selector unavailable")
	}
	return e.network.verify()
}

func (e *Engine) Stop() {
	if e.policy != nil {
		e.policy.udpMappings.reset(true)
		e.policy.mtproxies.cancel()
	}
	if e.network != nil {
		e.network.set(0)
	}
	if e.stack != nil {
		e.stack.Close()
		e.stack = nil
	}
}
