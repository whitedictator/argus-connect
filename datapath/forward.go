//go:build android || linux

package datapath

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	connectTimeout   = 12 * time.Second
	udpIdleTimeout   = 75 * time.Second
	firstReadTimeout = 2 * time.Second
	maximumTCPFlows  = 512
	maximumFirstData = 65535
)

var tcpCopyBuffers = sync.Pool{New: func() any {
	buffer := make([]byte, 64*1024)
	return &buffer
}}

func attachTCP(s *stack.Stack, dialer *net.Dialer, policy *lightPolicy) {
	forwarder := tcp.NewForwarder(s, 0, maximumTCPFlows, func(request *tcp.ForwarderRequest) {
		id := request.ID()
		var queue waiter.Queue
		endpoint, failure := request.CreateEndpoint(&queue)
		if failure != nil {
			request.Complete(true)
			return
		}
		request.Complete(false)
		client := gonet.NewTCPConn(&queue, endpoint)
		go handleTCP(
			client,
			dialer,
			net.IP(id.LocalAddress.AsSlice()),
			uint16(id.LocalPort),
			policy,
		)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)
}

func dialTCPForService(
	dialer *net.Dialer,
	policy *lightPolicy,
	service serviceID,
	destination net.IP,
	requestedPort uint16,
	allowAlternateTelegramPorts bool,
	alternateAddresses []net.IP,
	nat64Address net.IP,
) (net.Conn, string, error) {
	proxyFallback := func(failure error) (net.Conn, string, error) {
		if service == serviceTelegram && policy.enabled.Load() && allowAlternateTelegramPorts {
			if connection, route, wssErr := dialTelegramWebSocket(dialer, policy, destination); wssErr == nil {
				return connection, route, nil
			} else {
				failure = wssErr
			}
		}
		if service != serviceUnknown && policy.proxies.count() > 0 {
			if connection, route, proxyErr := policy.proxies.dial(dialer, destination, requestedPort); proxyErr == nil {
				return connection, route, nil
			} else {
				failure = proxyErr
			}
		}
		return nil, "", failure
	}
	if service == serviceTelegram && policy.enabled.Load() && allowAlternateTelegramPorts && policy.telegramWSS.Load() {
		if connection, route, err := dialTelegramWebSocket(dialer, policy, destination); err == nil {
			return connection, route, nil
		}
	}
	type route struct {
		address net.IP
		port    uint16
		label   string
	}
	routes := make([]route, 0, 16)
	seen := make(map[string]bool)
	addRoute := func(address net.IP, port uint16, label string) {
		if address == nil {
			return
		}
		key := net.JoinHostPort(address.String(), strconv.Itoa(int(port)))
		if seen[key] {
			return
		}
		seen[key] = true
		routes = append(routes, route{address, port, label})
	}
	addRoute(destination, requestedPort, "primary")
	for _, alternateAddress := range alternateAddresses {
		family := "v4"
		if alternateAddress.To4() == nil {
			family = "v6"
		}
		addRoute(alternateAddress, requestedPort, "alternate-"+family)
	}
	if nat64Address != nil {
		addRoute(nat64Address, requestedPort, "nat64-ipv6")
	}
	if service == serviceTelegram {
		for _, candidate := range telegramAlternates(destination) {
			addRoute(candidate.address, requestedPort, candidate.label)
		}
	}
	// Telegram officially serves MTProto TCP on 80, 443 and 5222. If the
	// carrier filters only the advertised port, race the other official
	// listeners instead of making the user wait through serial timeouts.
	if service == serviceTelegram && allowAlternateTelegramPorts {
		baseRoutes := append([]route(nil), routes...)
		for _, base := range baseRoutes {
			for _, port := range []uint16{443, 80, 5222} {
				if port != requestedPort {
					addRoute(base.address, port, base.label+"-port-"+strconv.Itoa(int(port)))
				}
			}
		}
	}

	timeout := connectTimeout
	if service != serviceUnknown {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	type dialResult struct {
		connection net.Conn
		failure    error
		route      string
	}
	results := make(chan dialResult)
	for _, candidate := range routes {
		candidate := candidate
		go func() {
			if delay := policy.routeStartDelay(service, candidate.label); delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			address := net.JoinHostPort(candidate.address.String(), strconv.Itoa(int(candidate.port)))
			connection, failure := dialer.DialContext(ctx, "tcp", address)
			policy.recordRoute(service, candidate.label, failure == nil)
			select {
			case results <- dialResult{connection, failure, candidate.label}:
			case <-ctx.Done():
				if connection != nil {
					_ = connection.Close()
				}
			}
		}()
	}
	var last error
	for range routes {
		select {
		case result := <-results:
			if result.failure == nil {
				cancel()
				return result.connection, result.route, nil
			}
			last = result.failure
		case <-ctx.Done():
			cancel()
			if last != nil {
				return proxyFallback(last)
			}
			return proxyFallback(ctx.Err())
		}
	}
	cancel()
	return proxyFallback(last)
}

func handleTCP(
	client net.Conn,
	dialer *net.Dialer,
	destination net.IP,
	destinationPort uint16,
	policy *lightPolicy,
) {
	defer client.Close()

	service := policy.classifyIP(destination)
	// Unknown traffic is forwarded immediately and remains untouched. Known
	// services wait briefly for their first client bytes so LIGHT can select a
	// safe strategy without delaying unrelated apps.
	// Cached Android DNS answers may predate this TUN, so ipHints alone cannot
	// identify a service. Inspect client-first TLS on 443 before bypassing LIGHT.
	if service == serviceUnknown && destinationPort != 443 && destinationPort != 5222 {
		upstream, _, err := dialTCPForService(dialer, policy, service, destination, destinationPort, false, nil, nil)
		if err != nil {
			return
		}
		tuneTCP(upstream)
		copyBothDirections(client, upstream, false)
		return
	}
	first, readErr := readFirstPayload(client)
	if readErr != nil && len(first) == 0 {
		upstream, route, err := dialTCPForService(
			dialer, policy, service, destination, destinationPort, false, nil, policy.nat64Address(destination),
		)
		if err != nil {
			policy.recordConnect(service, false, "")
			return
		}
		policy.recordConnect(service, true, route)
		tuneTCP(upstream)
		copyBothDirections(client, upstream, false)
		return
	}
	host := ""
	if detectedHost, _, ok := findTLSServerName(first); ok {
		host = detectedHost
		service = classifyDomain(detectedHost)
	}
	if isWhatsAppNoiseStart(first) {
		service = serviceWhatsApp
		host = "" // Noise is not TLS; never reuse an HTTPS host route.
	}
	if service == serviceTelegram && policy.enabled.Load() && policy.mtproxies.enabled() {
		if len(first) > 0 && first[0] != 0x16 && len(first) < 64 {
			client.SetReadDeadline(time.Now().Add(firstReadTimeout))
			tail := make([]byte, 64-len(first))
			n, _ := io.ReadFull(client, tail)
			first = append(first, tail[:n]...)
			client.SetReadDeadline(time.Time{})
		}
		if handleTelegramMTProxy(client, dialer, destination, first, policy) {
			return
		}
	}
	traceID := uint64(0)
	traceStage, traceRoute := "closed-before-response", ""
	var traceBytes int64
	if service == serviceYouTube || service == serviceTelegram || service == serviceSignal || service == serviceWhatsApp || service == serviceMeta || service == serviceTwitter || service == serviceGemini || service == serviceNotebook {
		traceID = policy.mtproxies.beginTrace(net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)))
		// Service metadata only, never URL paths, messages or TLS payloads.
		policy.mtproxies.trace(traceID, net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)), "classified", string(service)+"/"+host, 0)
		if service == serviceYouTube {
			shape := fmt.Sprintf("first-bytes=%d", len(first))
			if len(first) >= 9 && first[0] == 0x16 {
				shape += fmt.Sprintf("/record=%d/handshake=%d", readUint16(first, 3), int(first[6])<<16|int(first[7])<<8|int(first[8]))
			}
			policy.mtproxies.trace(traceID, net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)), "tls-shape", shape, 0)
		}
		if service == serviceWhatsApp && host == "" {
			shape := fmt.Sprintf("first-bytes=%d/noise=%t", len(first), isWhatsAppNoiseStart(first))
			intro := first
			if offset := whatsappEdgeOffset(first); offset > 0 {
				intro = first[offset:]
				shape += "/edge=true"
			}
			if len(intro) >= 4 && intro[0] == 'W' && intro[1] == 'A' {
				shape += fmt.Sprintf("/wa-version=%d.%d", intro[2], intro[3])
			}
			policy.mtproxies.trace(traceID, net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)), "protocol-shape", shape, 0)
		}
		defer func() {
			policy.mtproxies.trace(traceID, net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)), traceStage, traceRoute, traceBytes)
		}()
	}
	_, _, tls := findTLSServerName(first)
	alternates := policy.alternateAddresses(host, destination, 4)
	if tls && policy.enabled.Load() && destination.To4() == nil {
		// The userspace TCP handshake can succeed before an IPv6 upstream is
		// reachable, preventing Chrome's Happy Eyeballs IPv4 retry. Recover
		// using A records for the original SNI, never a related Google host.
		alternates = policy.withIPv4Fallback(host, alternates)
	}
	var upstream net.Conn
	var route string
	var err error
	verified, useVerified := policy.verifiedRoute(host, destinationPort)
	useVerified = useVerified && tls && policy.enabled.Load()
	useSignalDirect := tls && policy.enabled.Load() && policy.signalDirectTrial.Load() && signalDirectTrialHost(host, destinationPort)
	useSignalRelay := !useSignalDirect && tls && policy.enabled.Load() && policy.signalRelay.Load() && signalRelayHost(host, destinationPort)
	useWhatsAppWeb := policy.enabled.Load() && isWhatsAppNoiseStart(first)
	if useWhatsAppWeb {
		useVerified = false
		ctx, cancel := context.WithTimeout(policy.mtproxies.ctx, 5*time.Second)
		var routing []byte
		if offset := whatsappEdgeOffset(first); offset > 0 {
			routing = first[7:offset]
		}
		upstream, err = dialWhatsAppWeb(ctx, dialer, routing)
		cancel()
		route = "whatsapp-native-web/meta-edge"
	} else if useSignalDirect {
		useVerified = false
		ctx, cancel := context.WithTimeout(policy.mtproxies.ctx, 4*time.Second)
		upstream, err = dialer.DialContext(ctx, "tcp", "76.223.66.180:443")
		cancel()
		route = "signal-official-direct-trial"
	} else if useSignalRelay {
		useVerified = false
		ctx, cancel := context.WithTimeout(policy.mtproxies.ctx, 6*time.Second)
		upstream, route, err = dialSignalReflector(ctx, policy.mtproxies.ctx, dialer, policy.doh, host)
		if err != nil {
			upstream, err = dialSignalRelay(ctx, dialer)
			route = "signal-relay/sgpx.toomore.net/tls12"
		}
		cancel()
	} else if useVerified {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		upstream, err = dialer.DialContext(ctx, "tcp", verified.Address)
		cancel()
		if err == nil {
			route = "verified-host"
		} else {
			policy.reportVerifiedRoute(host, verified.Address, false, verified.Strategy)
			useVerified = false
		}
	}
	if !useVerified && !useSignalRelay && !useWhatsAppWeb && !useSignalDirect {
		upstream, route, err = dialTCPForService(dialer, policy, service, destination, destinationPort, !tls, alternates, policy.nat64Address(destination))
	}
	if err != nil {
		traceStage = "tcp-dial-failed"
		policy.recordConnect(service, false, "")
		return
	}
	defer upstream.Close()
	policy.recordConnect(service, true, route)
	tuneTCP(upstream)

	channel := channelPolicyKey(service, host)
	strategy := policy.chooseChannel(service, channel, first)
	if useSignalDirect {
		strategy = strategyDisorder
	}
	if useSignalRelay || useWhatsAppWeb {
		strategy = strategyDirect // Preserve inner TLS bytes exactly.
	}
	if useVerified {
		strategy = verified.Strategy
		policy.mu.Lock()
		policy.current[service] = strategy
		policy.mu.Unlock()
	}
	// Router-derived adaptive handshake retry. No alternate CDN, TLS MITM,
	// client request replay, 0-RTT duplication or change to other services.
	var initialServerBytes []byte
	helloSent := false
	if policy.enabled.Load() && service == serviceYouTube && safeYouTubeHello(first, host) {
		select {
		case youtubeHandshakeSlots <- struct{}{}:
			result, hedgeErr := raceYouTubeHello(policy.mtproxies.ctx, dialer, upstream, first, strategy)
			<-youtubeHandshakeSlots
			if hedgeErr != nil {
				upstream.Close()
				return
			}
			upstream = result.conn
			defer upstream.Close()
			strategy = result.strategy
			initialServerBytes = result.first
			helloSent = true
			if _, err = client.Write(initialServerBytes); err != nil {
				return
			}
		default: // Existing path remains available under concurrency pressure.
		}
	}
	traceRoute = route + "/" + string(strategy)
	if traceID != 0 {
		policy.mtproxies.trace(traceID, net.JoinHostPort(destination.String(), fmt.Sprint(destinationPort)), "tcp-connected", traceRoute, 0)
	}
	serverResult := make(chan int64, 1)
	go func() {
		count, _ := copyTCPMeasured(client, upstream, func(bytes int64, elapsed time.Duration) {
			policy.recordTraffic(service, route, strategy, bytes, elapsed)
			if channel != service {
				policy.recordTraffic(channel, route, strategy, bytes, elapsed)
			}
			if useVerified && bytes >= 32*1024 {
				policy.reportVerifiedRoute(host, verified.Address, true, verified.Strategy)
			}
		})
		serverResult <- count
	}()

	if len(first) > 0 && !helloSent {
		if err := applyStrategy(upstream, first, strategy); err != nil {
			if useVerified {
				policy.reportVerifiedRoute(host, verified.Address, false, verified.Strategy)
			}
			policy.proxies.report(route, false)
			policy.recordTCP(service, strategy, false)
			if channel != service {
				policy.recordTCP(channel, strategy, false)
			}
			return
		}
	}

	clientResult := make(chan int64, 1)
	go func() {
		count, _ := copyTCP(upstream, client)
		clientResult <- count
	}()
	serverBytes := int64(0)
	serverFinished := false
	select {
	case serverBytes = <-serverResult:
		serverFinished = true
	case <-clientResult:
	}
	_ = client.Close()
	_ = upstream.Close()
	if !serverFinished {
		serverBytes = <-serverResult
	}
	traceBytes = serverBytes
	traceStage = "stream-ended"
	success := meaningfulTCPResponse(service, serverBytes)
	if useVerified {
		// A small valid reply (e.g. YouTube robots.txt or API response) is not
		// a failed route. Throughput-promotion thresholds must not blacklist a
		// separately TLS/HTTP-verified endpoint. This is NOT application success.
		policy.reportVerifiedRoute(host, verified.Address, serverBytes > 0, verified.Strategy)
	}
	policy.proxies.report(route, success)
	policy.recordTCPOutcome(service, strategy, serverBytes)
	if channel != service {
		policy.recordTCPOutcome(channel, strategy, serverBytes)
	}
}

func meaningfulTCPResponse(service serviceID, serverBytes int64) bool {
	// A TLS alert or a throttling interstitial is often a few hundred bytes and
	// is not evidence that YouTube works. Only promote a YouTube strategy after
	// at least one traffic-meter-sized body was delivered.
	if service == serviceYouTube {
		return serverBytes >= 32*1024
	}
	return serverBytes > 0
}

func tuneTCP(connection net.Conn) {
	if tcpConnection, ok := connection.(*net.TCPConn); ok {
		_ = tcpConnection.SetNoDelay(true)
		_ = tcpConnection.SetReadBuffer(1024 * 1024)
		_ = tcpConnection.SetWriteBuffer(1024 * 1024)
		_ = tcpConnection.SetKeepAlive(true)
		_ = tcpConnection.SetKeepAlivePeriod(30 * time.Second)
	}
}

func copyTCP(destination, source net.Conn) (int64, error) {
	buffer := tcpCopyBuffers.Get().(*[]byte)
	defer tcpCopyBuffers.Put(buffer)
	return io.CopyBuffer(destination, source, *buffer)
}

type trafficMeter struct {
	destination io.Writer
	onSample    func(int64, time.Duration)
	started     time.Time
	bytes       int64
}

func (m *trafficMeter) Write(payload []byte) (int, error) {
	count, err := m.destination.Write(payload)
	m.bytes += int64(count)
	if elapsed := time.Since(m.started); m.bytes >= 32*1024 && elapsed >= time.Second {
		m.onSample(m.bytes, elapsed)
		m.bytes = 0
		m.started = time.Now()
	}
	return count, err
}

func (m *trafficMeter) flush() {
	if m.bytes >= 32*1024 {
		m.onSample(m.bytes, time.Since(m.started))
	}
}

// copyTCPMeasured publishes rolling throughput while a long-lived media flow
// is still active. Waiting until YouTube closes its connection can take many
// minutes and left the UI showing "not measured" during the actual video.
func copyTCPMeasured(destination, source net.Conn, onSample func(int64, time.Duration)) (int64, error) {
	buffer := tcpCopyBuffers.Get().(*[]byte)
	defer tcpCopyBuffers.Put(buffer)
	meter := &trafficMeter{destination: destination, onSample: onSample, started: time.Now()}
	count, err := io.CopyBuffer(meter, source, *buffer)
	meter.flush()
	return count, err
}

func readFirstPayload(source net.Conn) ([]byte, error) {
	_ = source.SetReadDeadline(time.Now().Add(firstReadTimeout))
	defer source.SetReadDeadline(time.Time{})
	buffer := make([]byte, maximumFirstData)
	count, err := source.Read(buffer)
	if count == 0 {
		return nil, err
	}
	data := append([]byte(nil), buffer[:count]...)
	if data[0] == 'E' {
		readTo := func(size int) error {
			for len(data) < size {
				n, e := source.Read(buffer[:size-len(data)])
				data = append(data, buffer[:n]...)
				if e != nil {
					return e
				}
				if n == 0 {
					return io.ErrNoProgress
				}
			}
			return nil
		}
		if e := readTo(4); e != nil {
			return data, e
		}
		if data[1] == 'D' && data[2] == 0 && data[3] == 1 {
			if e := readTo(7); e != nil {
				return data, e
			}
			n := int(data[4])<<16 | int(data[5])<<8 | int(data[6])
			if n <= 256 {
				if e := readTo(7 + n + 11); e != nil {
					return data, e
				}
			}
		}
	}
	// Native WhatsApp may send its small transport preface separately from
	// the Noise hello. TCP read boundaries are not protocol boundaries.
	if data[0] == 'W' && len(data) < 11 {
		for len(data) < 11 {
			if len(data) >= 2 && data[1] != 'A' {
				break
			}
			n, nextErr := source.Read(buffer[:11-len(data)])
			data = append(data, buffer[:n]...)
			if nextErr != nil {
				return data, nextErr
			}
			if n == 0 {
				return data, io.ErrNoProgress
			}
		}
	}
	// TCP can split even the five-byte TLS record header. Do not silently
	// classify such connections as non-TLS and miss the selected strategy.
	if data[0] == 0x16 && len(data) < 5 {
		for len(data) < 5 {
			n, nextErr := source.Read(buffer[:5-len(data)])
			data = append(data, buffer[:n]...)
			if nextErr != nil {
				return data, nextErr
			}
			if n == 0 {
				return data, io.ErrNoProgress
			}
		}
	}
	if len(data) >= 5 && data[0] == 0x16 {
		expected := 5 + readUint16(data, 3)
		if expected <= len(buffer) {
			for len(data) < expected {
				n, nextErr := source.Read(buffer[len(data):expected])
				data = append(data, buffer[len(data):len(data)+n]...)
				if nextErr != nil {
					return data, nextErr
				}
			}
		}
	}
	return data, err
}

func attachUDP(s *stack.Stack, dialer *net.Dialer, policy *lightPolicy) {
	forwarder := udp.NewForwarder(s, func(request *udp.ForwarderRequest) bool {
		id := request.ID()
		destination := net.IP(id.LocalAddress.AsSlice())
		service := policy.classifyIP(destination)
		if policy.shouldForceTCP(service, uint16(id.LocalPort)) {
			return false
		}
		var queue waiter.Queue
		endpoint, failure := request.CreateEndpoint(&queue)
		if failure != nil {
			return false
		}
		client := gonet.NewUDPConn(&queue, endpoint)
		if policy.enabled.Load() && id.LocalPort == 53 {
			go policy.doh.serve(client)
			return true
		}
		if policy.enabled.Load() && policy.voiceMappingTrial.Load() && (service == serviceTelegram || service == serviceUnknown) {
			sourceKey := net.JoinHostPort(net.IP(id.RemoteAddress.AsSlice()).String(), strconv.Itoa(int(id.RemotePort)))
			go forwardCallUDP(client, dialer, policy, service, destination, uint16(id.LocalPort), sourceKey)
		} else {
			upstreams := dialUDPRoutes(dialer, policy, service, destination, uint16(id.LocalPort))
			if len(upstreams) == 0 {
				client.Close()
				policy.recordUDP(service, false)
				return false
			}
			if policy.enabled.Load() && service != serviceUnknown {
				go relayMeasuredUDPRace(client, upstreams, policy, service)
			} else {
				go copyBothDirections(client, upstreams[0].connection, true)
			}
		}
		return true
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, forwarder.HandlePacket)
}

// Only voice/STUN sources use stable port mappings. QUIC and existing known
// service routes keep their previous path. Inspect one datagram without editing
// its bytes or boundaries; never collect the payload in diagnostics.
type firstDatagramConn struct {
	net.Conn
	first []byte
}

func (c *firstDatagramConn) Read(b []byte) (int, error) {
	if c.first != nil {
		data := c.first
		c.first = nil
		if len(b) < len(data) {
			return copy(b, data), io.ErrShortBuffer
		}
		return copy(b, data), nil
	}
	return c.Conn.Read(b)
}
func plainSTUNBinding(data []byte) bool {
	return len(data) >= 20 && data[0] == 0 && data[1] == 1 && readUint16(data, 2) == len(data)-20 &&
		len(data)%4 == 0 && data[4] == 0x21 && data[5] == 0x12 && data[6] == 0xa4 && data[7] == 0x42
}
func forwardCallUDP(client net.Conn, dialer *net.Dialer, p *lightPolicy, service serviceID, destination net.IP, port uint16, key string) {
	b := make([]byte, 65535)
	client.SetReadDeadline(time.Now().Add(udpIdleTimeout))
	n, err := client.Read(b)
	if err != nil {
		client.Close()
		return
	}
	client.SetReadDeadline(time.Time{})
	if service != serviceTelegram && !plainSTUNBinding(b[:n]) && !p.udpMappings.has(key) {
		key = ""
	}
	forwardUDP(&firstDatagramConn{Conn: client, first: b[:n]}, dialer, p, service, destination, port, key)
}
func forwardUDP(client net.Conn, dialer *net.Dialer, p *lightPolicy, service serviceID, destination net.IP, port uint16, key string) {
	upstreams := dialUDPRoutes(dialer, p, service, destination, port, key)
	if len(upstreams) == 0 {
		client.Close()
		p.recordUDP(service, false)
		return
	}
	if p.enabled.Load() && service != serviceUnknown {
		relayMeasuredUDPRace(client, upstreams, p, service)
	} else {
		copyBothDirections(client, upstreams[0].connection, true)
	}
}

type udpRoute struct {
	connection net.Conn
	label      string
}

func dialUDPRoutes(
	dialer *net.Dialer,
	policy *lightPolicy,
	service serviceID,
	destination net.IP,
	port uint16,
	mappingKey ...string,
) []udpRoute {
	type candidate struct {
		address net.IP
		label   string
	}
	candidates := []candidate{{destination, "primary"}}
	if service != serviceUnknown {
		for _, address := range policy.alternateAddressesForIP(destination, 3) {
			family := "v4"
			if address.To4() == nil {
				family = "v6"
			}
			candidates = append(candidates, candidate{address, "alternate-" + family})
		}
		if translated := policy.nat64Address(destination); translated != nil {
			candidates = append(candidates, candidate{translated, "nat64-ipv6"})
		}
	}
	seen := make(map[string]bool)
	routes := make([]udpRoute, 0, len(candidates))
	for _, candidate := range candidates {
		address := net.JoinHostPort(candidate.address.String(), strconv.Itoa(int(port)))
		if seen[address] {
			continue
		}
		seen[address] = true
		var connection net.Conn
		var err error
		if len(mappingKey) > 0 && mappingKey[0] != "" {
			connection, err = policy.udpMappings.dial(mappingKey[0], candidate.address, port)
		} else {
			connection, err = dialer.Dial("udp", address)
		}
		if err == nil {
			routes = append(routes, udpRoute{connection, candidate.label})
		}
	}
	return routes
}

func relayMeasuredUDPRace(client net.Conn, upstreams []udpRoute, policy *lightPolicy, service serviceID) {
	defer client.Close()
	defer func() {
		for _, upstream := range upstreams {
			_ = upstream.connection.Close()
		}
	}()
	// Only Telegram-owned destinations are traced; never peer addresses,
	// packet contents, ICE credentials, or audio. A reply is not a call proof.
	traceID := uint64(0)
	traceDestination := ""
	if service == serviceTelegram && len(upstreams) > 0 {
		traceDestination = upstreams[0].connection.RemoteAddr().String()
		traceID = policy.mtproxies.beginTrace(traceDestination)
	}
	replySignal := make(chan struct{}, 1)
	var replyOnce sync.Once
	selected := -1
	var selectedMu sync.RWMutex
	var clientWriteMu sync.Mutex
	for index, upstream := range upstreams {
		index, upstream := index, upstream
		go func() {
			buffer := make([]byte, 65535)
			trafficStarted := time.Now()
			trafficBytes := int64(0)
			defer func() {
				if trafficBytes >= 32*1024 {
					policy.recordTraffic(service, upstream.label, "", trafficBytes, time.Since(trafficStarted))
				}
			}()
			for {
				_ = upstream.connection.SetReadDeadline(time.Now().Add(udpIdleTimeout))
				count, err := upstream.connection.Read(buffer)
				if count > 0 {
					selectedMu.Lock()
					if selected == -1 {
						selected = index
						replyOnce.Do(func() {
							policy.recordUDPRoute(service, true, upstream.label)
							if traceID != 0 {
								policy.mtproxies.trace(traceID, traceDestination, "udp-first-reply", upstream.label, int64(count))
							}
							replySignal <- struct{}{}
						})
					}
					winner := selected == index
					selectedMu.Unlock()
					if winner {
						clientWriteMu.Lock()
						_, writeErr := client.Write(buffer[:count])
						clientWriteMu.Unlock()
						trafficBytes += int64(count)
						if elapsed := time.Since(trafficStarted); trafficBytes >= 32*1024 && elapsed >= time.Second {
							policy.recordTraffic(service, upstream.label, "", trafficBytes, elapsed)
							trafficBytes = 0
							trafficStarted = time.Now()
						}
						if writeErr != nil {
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
		}()
	}
	buffer := make([]byte, 65535)
	var firstWrite sync.Once
	for {
		_ = client.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		count, err := client.Read(buffer)
		if count > 0 {
			selectedMu.RLock()
			winner := selected
			selectedMu.RUnlock()
			writeSucceeded := false
			for index, upstream := range upstreams {
				if winner >= 0 && winner != index {
					continue
				}
				if _, writeErr := upstream.connection.Write(buffer[:count]); writeErr == nil {
					writeSucceeded = true
				}
			}
			if !writeSucceeded {
				break
			}
			firstWrite.Do(func() {
				if traceID != 0 {
					policy.mtproxies.trace(traceID, traceDestination, "udp-first-send", "telegram/call-not-verified", int64(count))
				}
				go func() {
					select {
					case <-replySignal:
					case <-time.After(3 * time.Second):
						policy.recordUDP(service, false)
						if traceID != 0 {
							policy.mtproxies.trace(traceID, traceDestination, "udp-no-reply-3s", "telegram/call-not-verified", 0)
						}
					}
				}()
			})
		}
		if err != nil {
			break
		}
	}
}

func copyBothDirections(left, right net.Conn, datagrams bool) {
	defer left.Close()
	defer right.Close()
	done := make(chan struct{}, 2)
	copyOne := func(destination, source net.Conn) {
		if datagrams {
			buffer := make([]byte, 65535)
			for {
				if source.SetReadDeadline(time.Now().Add(udpIdleTimeout)) != nil {
					break
				}
				count, err := source.Read(buffer)
				if count > 0 {
					if _, writeErr := destination.Write(buffer[:count]); writeErr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		} else {
			_, _ = copyTCP(destination, source)
		}
		done <- struct{}{}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	<-done
}

func flowDestination(id stack.TransportEndpointID) string {
	return net.JoinHostPort(net.IP(id.LocalAddress.AsSlice()).String(), strconv.Itoa(int(id.LocalPort)))
}
