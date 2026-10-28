package datapath

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type serviceID string

const (
	serviceUnknown  serviceID = "other"
	serviceTelegram serviceID = "telegram"
	serviceWhatsApp serviceID = "whatsapp"
	serviceYouTube  serviceID = "youtube"
	serviceSignal   serviceID = "signal"
	serviceThreema  serviceID = "threema"
	serviceMeta     serviceID = "meta"
	serviceTwitter  serviceID = "twitter"
	serviceGemini   serviceID = "gemini"
	serviceNotebook serviceID = "notebook"
)

type strategyID string

const (
	strategyDirect    strategyID = "direct"
	strategySplit1    strategyID = "tcp-split-1"
	strategySplit8    strategyID = "tcp-split-8"
	strategySplitMid  strategyID = "tcp-split-mid"
	strategySNISplit  strategyID = "sni-split"
	strategyTLSRecord strategyID = "tls-record-split"
	strategyTLSMulti  strategyID = "tls-multi-record"
	strategySNILabel  strategyID = "sni-label-split"
	strategyMicro     strategyID = "tcp-microburst"
	strategyMTProto   strategyID = "mtproto-stagger"
	strategyOOB       strategyID = "tcp-urgent-probe"
	strategyDisorder  strategyID = "tcp-lowttl-disorder"
	strategyTLSCombo  strategyID = "tls-record-sni-combo"
	strategyComboFast strategyID = "tls-record-sni-combo-fast"
	strategyComboMid  strategyID = "tls-record-sni-combo-balanced"
	strategyTLSEmpty  strategyID = "tls-empty-record-prefix"
	strategySmallMSS  strategyID = "tcp-small-mss"
)

type strategyResult struct {
	Attempts      int `json:"attempts"`
	Success       int `json:"success"`
	Failure       int `json:"failure"`
	RecentFailure int `json:"recentFailure"`
}

type udpResult struct {
	Attempts int    `json:"attempts"`
	Replies  int    `json:"replies"`
	Fallback bool   `json:"tcpFallback"`
	Route    string `json:"route,omitempty"`
}

type connectResult struct {
	Attempts  int    `json:"attempts"`
	Connected int    `json:"connected"`
	Route     string `json:"route,omitempty"`
}

type routeResult struct {
	Attempts  int `json:"attempts"`
	Connected int `json:"connected"`
}

type trafficResult struct {
	Samples int   `json:"samples"`
	Bytes   int64 `json:"bytes"
 "context"`
	LastKbps     int64                `json:"lastKbps"`
	RouteKbps    map[string]int64     `json:"routeKbps"`
	StrategyKbps map[strategyID]int64 `json:"strategyKbps"`
}

type lightPolicy struct {
	warmStrategies    map[serviceID]map[strategyID]bool
	enabled           atomic.Bool
	signalRelay       atomic.Bool
	signalDirectTrial atomic.Bool
	voiceMappingTrial atomic.Bool
	doh               *dohResolver
	proxies           proxyPool
	mtproxies         *mtProxyPool
	udpMappings       *udpMappingPool

	mu               sync.RWMutex
	ipHints          map[netip.Addr]serviceID
	domainHints      map[string][]netip.Addr
	routeRotation    map[string]int
	nat64Prefix      netip.Prefix
	scores           map[serviceID]map[strategyID]*strategyResult
	udp              map[serviceID]*udpResult
	connect          map[serviceID]*connectResult
	routes           map[serviceID]map[string]*routeResult
	traffic          map[serviceID]*trafficResult
	rotation         map[serviceID]int
	current          map[serviceID]strategyID
	verifiedServices map[string]verifiedServiceRoute
	telegramProbe    atomic.Uint32
	telegramWSS      atomic.Bool
}

func (p *lightPolicy) setNAT64Prefix(value string) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is6() || prefix.Bits() != 96 {
		prefix = netip.Prefix{}
	}
	p.mu.Lock()
	p.nat64Prefix = prefix
	p.mu.Unlock()
}

// nat64Address performs RFC 6052 /96 synthesis only when Android reported a
// real NAT64 prefix for the selected physical network. Arbitrary IPv6
// rewriting would create unroutable destinations and is deliberately refused.
func (p *lightPolicy) nat64Address(original net.IP) net.IP {
	v4 := original.To4()
	if v4 == nil {
		return nil
	}
	p.mu.RLock()
	prefix := p.nat64Prefix
	p.mu.RUnlock()
	if !prefix.IsValid() || prefix.Bits() != 96 {
		return nil
	}
	bytes := prefix.Addr().As16()
	copy(bytes[12:], v4)
	address := netip.AddrFrom16(bytes)
	return net.IP(address.AsSlice())
}

var telegramPrefixes = mustPrefixes(
	"91.108.4.0/22", "91.108.8.0/22", "91.108.12.0/22",
	"91.108.16.0/22", "91.108.56.0/22", "149.154.160.0/20",
	"2001:b28:f23d::/48", "2001:b28:f23f::/48", "2001:67c:4e8::/48",
	"2001:b28:f23c::/48", "2a0a:f280::/32",
)

func newLightPolicy(dialer *net.Dialer) *lightPolicy {
	p := &lightPolicy{
		ipHints:       make(map[netip.Addr]serviceID),
		domainHints:   make(map[string][]netip.Addr),
		routeRotation: make(map[string]int),
		scores:        make(map[serviceID]map[strategyID]*strategyResult),
		udp:           make(map[serviceID]*udpResult),
		connect:       make(map[serviceID]*connectResult),
		routes:        make(map[serviceID]map[string]*routeResult),
		traffic:       make(map[serviceID]*trafficResult),
		rotation:      make(map[serviceID]int),
		current:       make(map[serviceID]strategyID),
	}
	p.doh = newDoHResolver(dialer, p.observeDNS)
	p.mtproxies = newMTProxyPool()
	p.udpMappings = newUDPMappingPool(dialer)
	context.AfterFunc(p.mtproxies.ctx, func() { p.udpMappings.reset(true) })
	return p
}

func (p *lightPolicy) recordTraffic(
	service serviceID,
	route string,
	strategy strategyID,
	bytes int64,
	elapsed time.Duration,
) {
	if service == serviceUnknown || bytes < 32*1024 || elapsed.Milliseconds() < 1 {
		return
	}
	kbps := bytes * 8 / elapsed.Milliseconds()
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.traffic[service]
	if result == nil {
		result = &trafficResult{
			RouteKbps:    make(map[string]int64),
			StrategyKbps: make(map[strategyID]int64),
		}
		p.traffic[service] = result
	}
	if result.RouteKbps == nil {
		result.RouteKbps = make(map[string]int64)
	}
	if result.StrategyKbps == nil {
		result.StrategyKbps = make(map[strategyID]int64)
	}
	result.Samples++
	result.Bytes += bytes
	result.LastKbps = kbps
	result.RouteKbps[route] = kbps
	if strategy != "" {
		result.StrategyKbps[strategy] = kbps
	}
	if result.Samples >= 100 {
		result.Samples /= 2
		result.Bytes /= 2
	}
}

func (p *lightPolicy) routeStartDelay(service serviceID, route string) time.Duration {
	if service != serviceYouTube {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := p.traffic[service]
	if result == nil {
		return 0
	}
	current, known := result.RouteKbps[route]
	bestRoute, best := "", int64(0)
	for candidate, kbps := range result.RouteKbps {
		if kbps > best {
			bestRoute, best = candidate, kbps
		}
	}
	if best >= 2_500 && route != bestRoute {
		return 180 * time.Millisecond
	}
	if known && current < 1_000 {
		return 300 * time.Millisecond
	}
	if known && current < 2_500 {
		return 120 * time.Millisecond
	}
	return 0
}

func (p *lightPolicy) recordRoute(service serviceID, route string, connected bool) {
	if service == serviceUnknown || route == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	serviceRoutes := p.routes[service]
	if serviceRoutes == nil {
		serviceRoutes = make(map[string]*routeResult)
		p.routes[service] = serviceRoutes
	}
	result := serviceRoutes[route]
	if result == nil {
		result = &routeResult{}
		serviceRoutes[route] = result
	}
	result.Attempts++
	if connected {
		result.Connected++
	}
	if result.Attempts >= 200 {
		result.Attempts /= 2
		result.Connected /= 2
	}
}

func (p *lightPolicy) classifyIP(ip net.IP) serviceID {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return serviceUnknown
	}
	address = address.Unmap()
	for _, prefix := range telegramPrefixes {
		if prefix.Contains(address) {
			return serviceTelegram
		}
	}
	p.mu.RLock()
	service := p.ipHints[address]
	p.mu.RUnlock()
	if service == "" {
		return serviceUnknown
	}
	return service
}

func classifyDomain(host string) serviceID {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "notebook.google.com" {
		return serviceNotebook
	}
	if isGeminiDNSName(host) {
		return serviceGemini
	}
	// Exact CDN hosts seen failing in Facebook and independently verified
	// with disorder on this carrier. Do not move all shared fbcdn traffic.
	if host == "scontent-hel3-1.xx.fbcdn.net" || host == "scontent-arn2-1.xx.fbcdn.net" {
		return serviceMeta
	}
	for _, suffix := range []string{"x.com", "twitter.com", "twimg.com", "t.co"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceTwitter
		}
	}
	// Keep Meta social scoring separate from WhatsApp's working transport
	// and CDN routes. Shared fbcdn handling intentionally remains unchanged.
	for _, suffix := range []string{"facebook.com", "facebook.net", "instagram.com", "cdninstagram.com", "fbsbx.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceMeta
		}
	}
	telegram := []string{
		"telegram.org", "telegram.me", "t.me", "telegra.ph", "telesco.pe",
		"telegram-cdn.org", "telegram.space",
	}
	whatsApp := []string{
		"whatsapp.com", "whatsapp.net", "wa.me", "whatsapp-plus.info", "fbcdn.net",
	}
	youTube := []string{
		"youtube.com", "youtu.be", "googlevideo.com", "ytimg.com",
		"youtube-nocookie.com", "ggpht.com", "youtubei.googleapis.com", "youtube.googleapis.com",
	}
	signal := []string{
		"signal.org", "signal.art", "signal.group", "signal.link", "signal.me", "signal.tube",
	}
	threema := []string{
		"threema.ch", "threema.com",
	}
	for _, suffix := range telegram {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceTelegram
		}
	}
	for _, suffix := range whatsApp {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceWhatsApp
		}
	}
	for _, suffix := range youTube {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceYouTube
		}
	}
	for _, suffix := range signal {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceSignal
		}
	}
	for _, suffix := range threema {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return serviceThreema
		}
	}
	return serviceUnknown
}

func (p *lightPolicy) observeDNS(domain string, addresses []net.IP) {
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	service := classifyDomain(domain)
	if service == serviceUnknown {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ip := range addresses {
		if address, ok := netip.AddrFromSlice(ip); ok {
			address = address.Unmap()
			p.ipHints[address] = service
			known := p.domainHints[domain]
			if !containsAddr(known, address) {
				p.domainHints[domain] = append(known, address)
			}
		}
	}
}

func containsAddr(values []netip.Addr, candidate netip.Addr) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// alternateAddress returns another address from the same DNS answer set for
// the exact TLS hostname. The original SNI and encrypted payload are retained,
// so certificate validation still belongs to the application. Nothing is
// persisted or mixed across service/domain boundaries.
func (p *lightPolicy) alternateAddress(host string, original net.IP) net.IP {
	values := p.alternateAddresses(host, original, 1)
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func (p *lightPolicy) alternateAddresses(host string, original net.IP, limit int) []net.IP {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || limit < 1 {
		return nil
	}
	originalAddr, _ := netip.AddrFromSlice(original)
	originalAddr = originalAddr.Unmap()
	p.mu.Lock()
	defer p.mu.Unlock()
	values := p.domainHints[host]
	// The original destination may come from the app's own DNS cache and
	// need not occur in our answer set. A single different address is useful.
	if len(values) == 0 {
		return nil
	}
	start := p.routeRotation[host] % len(values)
	p.routeRotation[host]++
	result := make([]net.IP, 0, min(limit, len(values)))
	for offset := range values {
		candidate := values[(start+offset)%len(values)]
		if candidate != originalAddr && candidate.IsGlobalUnicast() {
			result = append(result, net.IP(candidate.AsSlice()))
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func (p *lightPolicy) alternateAddressesForIP(original net.IP, limit int) []net.IP {
	originalAddr, ok := netip.AddrFromSlice(original)
	if !ok || limit < 1 {
		return nil
	}
	originalAddr = originalAddr.Unmap()
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]net.IP, 0, limit)
	seen := map[netip.Addr]bool{originalAddr: true}
	for _, values := range p.domainHints {
		if !containsAddr(values, originalAddr) {
			continue
		}
		for _, candidate := range values {
			candidate = candidate.Unmap()
			if seen[candidate] || !candidate.IsGlobalUnicast() {
				continue
			}
			seen[candidate] = true
			result = append(result, net.IP(candidate.AsSlice()))
			if len(result) == limit {
				return result
			}
		}
	}
	return result
}

func (p *lightPolicy) choose(service serviceID, payload []byte) strategyID {
	if !p.enabled.Load() || service == serviceUnknown {
		return strategyDirect
	}
	_, _, tls := findTLSServerName(payload)
	candidates := []strategyID{
		strategyDirect, strategyMTProto, strategyMicro, strategySplit1, strategySplit8,
		strategySplitMid, strategySmallMSS, strategyOOB, strategyDisorder,
	}
	if tls {
		candidates = []strategyID{
			strategyDirect, strategyTLSRecord, strategySNISplit, strategySNILabel,
			strategyTLSMulti, strategyComboFast, strategyComboMid, strategyTLSCombo,
			strategyTLSEmpty, strategyMicro, strategyMTProto,
			strategySplit1, strategySplit8, strategySplitMid, strategySmallMSS,
			strategyOOB, strategyDisorder,
		}
		// Field-verified ordering only: still test direct first and retain every
		// fallback. HTTP success is learned on this network, never preloaded.
		preferred := strategyComboFast
		if basePolicyService(service) == serviceWhatsApp {
			preferred = strategyOOB
		}
		if basePolicyService(service) == serviceMeta {
			preferred = strategyDisorder
		}
		ordered := []strategyID{strategyDirect, preferred}
		if service == serviceGemini {
			// Chrome-like ClientHello to COMSS failed direct on the phone;
			// combo-fast and disorder both completed verified HTTP/2.
			ordered = []strategyID{strategyComboFast, strategyDisorder, strategyDirect}
		}
		for _, c := range candidates {
			if c != strategyDirect && c != preferred && !(service == serviceGemini && c == strategyDisorder) {
				ordered = append(ordered, c)
			}
		}
		candidates = ordered
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	serviceScores := p.scores[service]
	if serviceScores == nil {
		serviceScores = make(map[strategyID]*strategyResult)
		p.scores[service] = serviceScores
	}
	start := p.rotation[service] % len(candidates)
	p.rotation[service]++
	best := candidates[start]
	bestScore := -1.0
	hasSuccess := false
	for offset := range candidates {
		candidate := candidates[(start+offset)%len(candidates)]
		result := serviceScores[candidate]
		if result == nil {
			continue
		}
		hasSuccess = hasSuccess || (result.Success > 0 && result.RecentFailure < 2)
		// Laplace smoothing keeps selection adaptive with small samples.
		score := float64(result.Success+1)/float64(result.Attempts+2) - float64(result.RecentFailure)*0.35
		// A method that merely opens a connection is not necessarily the best
		// video method. Once real application traffic supplies a measurement,
		// give a bounded preference to the strategies that delivered it faster.
		if traffic := p.traffic[service]; traffic != nil {
			if kbps := traffic.StrategyKbps[candidate]; kbps > 0 {
				speedBonus := min(float64(kbps)/5_000.0, 1.0) * 0.45
				score += speedBonus
			}
		}
		if score > bestScore {
			best, bestScore = candidate, score
		}
	}
	// Explore only while no method works. Sacrificing every fifth LIVE flow to
	// an untested method caused avoidable reconnects and stalled media chunks.
	// A proven method remains selected until observed failures invalidate it;
	// future speed exploration belongs on independent diagnostic connections.
	selected := best
	if !hasSuccess {
		for _, candidate := range candidates {
			if serviceScores[candidate] == nil || serviceScores[candidate].Attempts == 0 {
				selected = candidate
				p.current[service] = selected
				return selected
			}
		}
	}
	if bestScore < 0 {
		selected = candidates[0]
	}
	p.current[service] = selected
	return selected
}

// Short encrypted YouTube replies are inconclusive: neither promote an
// unproven method nor evict one that delivered video on other connections.
func (p *lightPolicy) recordTCPOutcome(service serviceID, strategy strategyID, received int64) {
	if basePolicyService(service) == serviceYouTube && received > 0 && received < 32*1024 {
		return
	}
	p.recordTCP(service, strategy, received > 0)
}

func (p *lightPolicy) recordTCP(service serviceID, strategy strategyID, success bool) {
	if service == serviceUnknown {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	serviceScores := p.scores[service]
	if serviceScores == nil {
		serviceScores = make(map[strategyID]*strategyResult)
		p.scores[service] = serviceScores
	}
	result := serviceScores[strategy]
	if result == nil {
		result = &strategyResult{}
		serviceScores[strategy] = result
	}
	result.Attempts++
	if success {
		result.Success++
		result.RecentFailure = 0
	} else {
		result.Failure++
		result.RecentFailure++
	}
	// Bound counters without losing their ratio.
	if result.Attempts >= 200 {
		result.Attempts /= 2
		result.Success /= 2
		result.Failure = result.Attempts - result.Success
	}
}

func (p *lightPolicy) recordUDP(service serviceID, replied bool) {
	p.recordUDPRoute(service, replied, "")
}

func (p *lightPolicy) recordUDPRoute(service serviceID, replied bool, route string) {
	if service == serviceUnknown {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.udp[service]
	if result == nil {
		result = &udpResult{}
		p.udp[service] = result
	}
	result.Attempts++
	if replied {
		result.Replies++
		result.Fallback = false
		if route != "" {
			result.Route = route
		}
	} else if service != serviceTelegram && result.Attempts >= 2 && result.Replies == 0 {
		result.Fallback = true
	}
	if result.Attempts >= 40 {
		result.Attempts /= 2
		result.Replies /= 2
	}
}

func (p *lightPolicy) recordConnect(service serviceID, connected bool, route string) {
	if service == serviceUnknown {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.connect[service]
	if result == nil {
		result = &connectResult{}
		p.connect[service] = result
	}
	result.Attempts++
	if connected {
		result.Connected++
		result.Route = route
	}
	if result.Attempts >= 100 {
		result.Attempts /= 2
		result.Connected /= 2
	}
}

func (p *lightPolicy) shouldForceTCP(service serviceID, port uint16) bool {
	if !p.enabled.Load() || service == serviceUnknown {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := p.udp[service]
	if service == serviceYouTube || service == serviceMeta || service == serviceTwitter || service == serviceGemini || service == serviceNotebook {
		// Let QUIC prove itself first; fall back only after repeated UDP flows
		// receive no reply. Unconditionally disabling QUIC made working video
		// needlessly slow on networks where only the TLS path was filtered.
		return port == 443 && result != nil && result.Fallback
	}
	if service == serviceTelegram || service == serviceThreema || service == serviceSignal {
		// Voice uses UDP (including port 443). Dropping a datagram does not
		// convert it to TCP. In particular, failed ICE candidates must not
		// disable all remaining Telegram candidates until LIGHT restarts.
		return false
	}
	return port == 443 || (result != nil && result.Fallback)
}

func (p *lightPolicy) statusJSON() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	payload := struct {
		Enabled           bool                                         `json:"enabled"`
		DNS               bool                                         `json:"doh"`
		DNSTransport      string                                       `json:"dnsTransport"`
		DNSAttempts       int64                                        `json:"dohAttempts"`
		DNSSuccess        int64                                        `json:"dohSuccess"`
		GeminiAttempts    int64                                        `json:"geminiDnsAttempts"`
		GeminiSuccess     int64                                        `json:"geminiDnsSuccess"`
		GoogleDNS         map[string]int                               `json:"googleDnsQueries"`
		Scores            map[serviceID]map[strategyID]*strategyResult `json:"strategies"`
		UDP               map[serviceID]*udpResult                     `json:"udp"`
		Connect           map[serviceID]*connectResult                 `json:"connect"`
		Routes            map[serviceID]map[string]*routeResult        `json:"routes"`
		Traffic           map[serviceID]*trafficResult                 `json:"traffic"`
		Current           map[serviceID]strategyID                     `json:"current"`
		Hints             int                                          `json:"learnedIpHints"`
		NAT64             bool                                         `json:"nat64"`
		TelegramWSS       bool                                         `json:"telegramWss"`
		VoiceMappingTrial bool                                         `json:"voiceMappingTrial"`
		Proxies           json.RawMessage                              `json:"proxies"`
		MTProxies         json.RawMessage                              `json:"mtproxies"`
	}{
		p.enabled.Load(), p.doh.healthy.Load(), p.doh.activeTransport(), p.doh.attempts.Load(), p.doh.successes.Load(),
		p.doh.geminiAttempts.Load(), p.doh.geminiSuccess.Load(), p.doh.googleDNSDiagnostics(),
		p.scores, p.udp, p.connect, p.routes, p.traffic, p.current, len(p.ipHints), p.nat64Prefix.IsValid(),
		p.telegramWSS.Load(), p.voiceMappingTrial.Load(), p.proxies.statusJSON(), p.mtproxies.statusJSON(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"enabled":true,"error":"status"}`
	}
	return string(encoded)
}

func (p *lightPolicy) loadState(encoded string) {
	var saved struct {
		Scores map[serviceID]map[strategyID]*strategyResult `json:"strategies"`
		UDP    map[serviceID]*udpResult                     `json:"udp"`
	}
	if json.Unmarshal([]byte(encoded), &saved) != nil {
		return
	}
	allowedServices := map[serviceID]bool{
		serviceTelegram: true, serviceWhatsApp: true, serviceYouTube: true,
		serviceSignal: true, serviceThreema: true, serviceMeta: true, serviceTwitter: true,
		serviceGemini: true, serviceNotebook: true,
		"youtube/video": true, "youtube/feed": true, "youtube/thumbnails": true,
		"whatsapp/messages": true, "whatsapp/media": true,
		"signal/messages": true, "signal/media": true, "signal/calls": true,
		"twitter/feed": true, "twitter/photos": true, "twitter/video": true, "twitter/assets": true,
		"meta/web": true, "meta/media": true,
	}
	allowedStrategies := map[strategyID]bool{
		strategyDirect: true, strategySplit1: true, strategySplit8: true,
		strategySplitMid: true, strategySNISplit: true, strategyTLSRecord: true,
		strategyTLSMulti: true, strategySNILabel: true, strategyMicro: true, strategyMTProto: true,
		strategyOOB: true, strategyDisorder: true, strategyTLSCombo: true,
		strategyComboFast: true, strategyComboMid: true,
		strategyTLSEmpty: true, strategySmallMSS: true,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for service, values := range saved.Scores {
		if !allowedServices[service] {
			continue
		}
		for strategy, result := range values {
			if !allowedStrategies[strategy] || result == nil || result.Attempts < 0 || result.Attempts > 200 {
				continue
			}
			if result.Success < 0 || result.Success > result.Attempts {
				continue
			}
			if result.RecentFailure < 0 || result.RecentFailure > result.Attempts {
				result.RecentFailure = 0
			}
			// A failed strategy on yesterday's Wi-Fi must not poison today's
			// carrier session. Restore only evidence that a method has worked,
			// and strongly decay it so every new LIGHT run rechecks the path.
			if result.Success == 0 {
				continue
			}
			result.Attempts = min(result.Attempts, 8)
			result.Success = min(result.Success, result.Attempts)
			result.Failure = result.Attempts - result.Success
			result.RecentFailure = min(result.RecentFailure, 1)
			if p.scores[service] == nil {
				p.scores[service] = make(map[strategyID]*strategyResult)
			}
			p.scores[service][strategy] = result
			if p.warmStrategies == nil {
				p.warmStrategies = make(map[serviceID]map[strategyID]bool)
			}
			if p.warmStrategies[service] == nil {
				p.warmStrategies[service] = make(map[strategyID]bool)
			}
			p.warmStrategies[service][strategy] = true
		}
	}
	for service, result := range saved.UDP {
		if !allowedServices[service] || result == nil || result.Attempts < 0 || result.Attempts > 40 {
			continue
		}
		if result.Replies < 0 || result.Replies > result.Attempts {
			continue
		}
		// A carrier that dropped QUIC yesterday must not force today's network
		// onto TCP before the new path gets its own bounded UDP race.
		if result.Replies == 0 {
			continue
		}
		result.Attempts = min(result.Attempts, 4)
		result.Replies = min(result.Replies, result.Attempts)
		result.Fallback = false
		p.udp[service] = result
	}
}

func applyStrategy(upstream net.Conn, payload []byte, strategy strategyID) error {
	if tcpConn, ok := upstream.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}
	switch strategy {
	case strategyTLSCombo:
		_, start, end, ok := findTLSServerNameBounds(payload)
		if ok {
			points := []int{start, start + (end-start)/2}
			if fragments, splitOK := tlsRecordFragments(payload, points); splitOK {
				return writeFragmentsTuned(upstream, fragments, []int{1, 3, 8}, 3*time.Millisecond, 5*time.Millisecond)
			}
		}
	case strategyComboFast:
		_, start, end, ok := findTLSServerNameBounds(payload)
		if ok {
			points := []int{start, start + (end-start)/2}
			if fragments, splitOK := tlsRecordFragments(payload, points); splitOK {
				return writeFragmentsTuned(upstream, fragments, []int{1, 8}, time.Millisecond, time.Millisecond)
			}
		}
	case strategyComboMid:
		_, start, end, ok := findTLSServerNameBounds(payload)
		if ok {
			points := []int{start, start + (end-start)/2}
			if fragments, splitOK := tlsRecordFragments(payload, points); splitOK {
				return writeFragmentsTuned(upstream, fragments, []int{1, 3, 8}, 2*time.Millisecond, 2*time.Millisecond)
			}
		}
	case strategyTLSEmpty:
		return writeEmptyTLSRecord(upstream, payload)
	case strategyTLSMulti:
		_, start, end, ok := findTLSServerNameBounds(payload)
		if ok {
			points := []int{start, start + (end-start)/2}
			if dot := strings.LastIndex(string(payload[start:end]), "."); dot > 0 {
				points[1] = start + dot
			}
			if fragments, splitOK := tlsRecordFragments(payload, points); splitOK {
				return writeFragments(upstream, fragments, 6*time.Millisecond)
			}
		}
	case strategyTLSRecord:
		_, hostOffset, ok := findTLSServerName(payload)
		if ok {
			rewritten, boundary, splitOK := splitTLSRecord(payload, hostOffset)
			if splitOK {
				return writeSplit(upstream, rewritten, boundary)
			}
		}
	case strategySNISplit:
		_, hostOffset, ok := findTLSServerName(payload)
		if ok {
			return writeSplit(upstream, payload, hostOffset)
		}
	case strategySNILabel:
		_, start, end, ok := findTLSServerNameBounds(payload)
		if ok {
			if dot := strings.LastIndex(string(payload[start:end]), "."); dot > 0 {
				return writeSplit(upstream, payload, start+dot)
			}
		}
	case strategyMicro:
		return writeSlices(upstream, payload, []int{1, 3, 8}, 4*time.Millisecond)
	case strategyMTProto:
		return writeSlices(upstream, payload, []int{1, 4, 16, 32}, 7*time.Millisecond)
	case strategyOOB:
		return writeUrgentProbe(upstream, payload)
	case strategyDisorder:
		return writeLowTTLDisorder(upstream, payload)
	case strategySmallMSS:
		return writeSmallMSS(upstream, payload)
	case strategySplit1:
		return writeSplit(upstream, payload, 1)
	case strategySplit8:
		return writeSplit(upstream, payload, 8)
	case strategySplitMid:
		return writeSplit(upstream, payload, min(len(payload)/2, 32))
	}
	return writeAll(upstream, payload)
}

func writeEmptyTLSRecord(destination net.Conn, payload []byte) error {
	if len(payload) < 5 || payload[0] != 0x16 {
		return writeAll(destination, payload)
	}
	empty := []byte{payload[0], payload[1], payload[2], 0, 0}
	if err := writeAll(destination, empty); err != nil {
		return err
	}
	time.Sleep(4 * time.Millisecond)
	return writeAll(destination, payload)
}

func writeSmallMSS(destination net.Conn, payload []byte) error {
	tcp, ok := destination.(*net.TCPConn)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	original := 0
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		original, optionErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG)
		if optionErr == nil {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 88)
		}
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return optionErr
	}
	defer func() {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, original)
		})
	}()
	return writeSlices(destination, payload, []int{1, 24, 64}, 2*time.Millisecond)
}

func writeFragmentsTuned(
	destination net.Conn,
	fragments [][]byte,
	slicePoints []int,
	slicePause time.Duration,
	recordPause time.Duration,
) error {
	for index, fragment := range fragments {
		if err := writeSlices(destination, fragment, slicePoints, slicePause); err != nil {
			return err
		}
		if index+1 < len(fragments) {
			time.Sleep(recordPause)
		}
	}
	return nil
}

// writeUrgentProbe places one duplicate byte outside the normal byte stream,
// then sends the untouched application payload. Some middleboxes include the
// urgent byte while endpoints consume only the ordinary stream. If the socket
// does not support TCP urgent data, this strategy fails and the policy moves on.
func writeUrgentProbe(destination net.Conn, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	tcp, ok := destination.(*net.TCPConn)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	if err := raw.Write(func(fd uintptr) bool {
		_, sendErr = syscall.SendmsgN(int(fd), payload[:1], nil, nil, syscall.MSG_OOB)
		return sendErr != syscall.EAGAIN && sendErr != syscall.EWOULDBLOCK
	}); err != nil {
		return err
	}
	if sendErr != nil {
		return sendErr
	}
	time.Sleep(4 * time.Millisecond)
	return writeAll(destination, payload)
}

// writeLowTTLDisorder briefly queues the first bytes with a hop limit that is
// too small for a mobile Internet path, then restores the socket and sends the
// remainder. TCP retransmission can reconstruct the original stream at the
// endpoint while a path observer sees a different arrival pattern. The method
// is deliberately a single bounded candidate because it can add one RTO.
func writeLowTTLDisorder(destination net.Conn, payload []byte) error {
	if len(payload) < 2 {
		return writeAll(destination, payload)
	}
	tcp, ok := destination.(*net.TCPConn)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	family := syscall.AF_INET
	if address, ok := tcp.RemoteAddr().(*net.TCPAddr); ok && address.IP.To4() == nil {
		family = syscall.AF_INET6
	}
	level, option := syscall.IPPROTO_IP, syscall.IP_TTL
	if family == syscall.AF_INET6 {
		level, option = syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS
	}
	original := 64
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		if value, getErr := syscall.GetsockoptInt(int(fd), level, option); getErr == nil && value > 0 {
			original = value
		}
		optionErr = syscall.SetsockoptInt(int(fd), level, option, 2)
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return optionErr
	}
	restored := false
	defer func() {
		if !restored {
			_ = raw.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), level, option, original)
			})
		}
	}()
	boundary := min(len(payload)-1, 8)
	if err := writeAll(destination, payload[:boundary]); err != nil {
		return err
	}
	time.Sleep(12 * time.Millisecond)
	if err := raw.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(int(fd), level, option, original)
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return optionErr
	}
	restored = true
	return writeAll(destination, payload[boundary:])
}

func writeSlices(destination net.Conn, payload []byte, points []int, pause time.Duration) error {
	start := 0
	for _, point := range points {
		if point <= start || point >= len(payload) {
			continue
		}
		if err := writeAll(destination, payload[start:point]); err != nil {
			return err
		}
		start = point
		time.Sleep(pause)
	}
	return writeAll(destination, payload[start:])
}

func writeFragments(destination net.Conn, fragments [][]byte, pause time.Duration) error {
	for index, fragment := range fragments {
		if err := writeAll(destination, fragment); err != nil {
			return err
		}
		if index+1 < len(fragments) {
			time.Sleep(pause)
		}
	}
	return nil
}

func writeSplit(destination net.Conn, payload []byte, boundary int) error {
	if boundary <= 0 || boundary >= len(payload) {
		return writeAll(destination, payload)
	}
	if err := writeAll(destination, payload[:boundary]); err != nil {
		return err
	}
	time.Sleep(8 * time.Millisecond)
	return writeAll(destination, payload[boundary:])
}

func writeAll(destination io.Writer, payload []byte) error {
	for len(payload) > 0 {
		count, err := destination.Write(payload)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		payload = payload[count:]
	}
	return nil
}

// findTLSServerName returns the SNI host and its absolute byte offset inside a
// complete first TLS record. It does not retain or log the host.
func findTLSServerName(packet []byte) (string, int, bool) {
	host, start, end, ok := findTLSServerNameBounds(packet)
	return host, start + (end-start)/2, ok
}

func findTLSServerNameBounds(packet []byte) (string, int, int, bool) {
	if len(packet) < 9 || packet[0] != 0x16 || packet[5] != 0x01 {
		return "", 0, 0, false
	}
	recordEnd := 5 + readUint16(packet, 3)
	if recordEnd > len(packet) || recordEnd < 9+2+32+1 {
		return "", 0, 0, false
	}
	position := 9 + 2 + 32
	if position >= recordEnd {
		return "", 0, 0, false
	}
	position += 1 + int(packet[position])
	if position+2 > recordEnd {
		return "", 0, 0, false
	}
	position += 2 + readUint16(packet, position)
	if position >= recordEnd {
		return "", 0, 0, false
	}
	position += 1 + int(packet[position])
	if position+2 > recordEnd {
		return "", 0, 0, false
	}
	extensionsEnd := position + 2 + readUint16(packet, position)
	position += 2
	if extensionsEnd > recordEnd {
		return "", 0, 0, false
	}
	for position+4 <= extensionsEnd {
		typeID := readUint16(packet, position)
		length := readUint16(packet, position+2)
		position += 4
		if position+length > extensionsEnd {
			return "", 0, 0, false
		}
		if typeID == 0 && length >= 5 {
			listEnd := position + 2 + readUint16(packet, position)
			namePosition := position + 2
			for namePosition+3 <= listEnd && listEnd <= position+length {
				nameType := packet[namePosition]
				nameLength := readUint16(packet, namePosition+1)
				namePosition += 3
				if namePosition+nameLength > listEnd {
					return "", 0, 0, false
				}
				if nameType == 0 && nameLength > 0 {
					host := string(packet[namePosition : namePosition+nameLength])
					return strings.ToLower(host), namePosition, namePosition + nameLength, true
				}
				namePosition += nameLength
			}
		}
		position += length
	}
	return "", 0, 0, false
}

func tlsRecordFragments(packet []byte, points []int) ([][]byte, bool) {
	if len(packet) < 5 {
		return nil, false
	}
	recordEnd := 5 + readUint16(packet, 3)
	if recordEnd > len(packet) {
		return nil, false
	}
	valid := make([]int, 0, len(points)+2)
	valid = append(valid, 5)
	for _, point := range points {
		if point > valid[len(valid)-1] && point < recordEnd {
			valid = append(valid, point)
		}
	}
	valid = append(valid, recordEnd)
	if len(valid) < 3 {
		return nil, false
	}
	fragments := make([][]byte, 0, len(valid)-1)
	for index := 0; index+1 < len(valid); index++ {
		start, end := valid[index], valid[index+1]
		header := append([]byte(nil), packet[:5]...)
		length := end - start
		header[3], header[4] = byte(length>>8), byte(length)
		fragment := append(header, packet[start:end]...)
		if end == recordEnd {
			fragment = append(fragment, packet[recordEnd:]...)
		}
		fragments = append(fragments, fragment)
	}
	return fragments, true
}

func splitTLSRecord(packet []byte, splitAt int) ([]byte, int, bool) {
	if len(packet) < 5 || splitAt <= 5 {
		return nil, 0, false
	}
	recordLength := readUint16(packet, 3)
	recordEnd := 5 + recordLength
	if recordEnd > len(packet) || splitAt >= recordEnd {
		return nil, 0, false
	}
	leftLength := splitAt - 5
	rightLength := recordEnd - splitAt
	leftHeader := append([]byte(nil), packet[:5]...)
	leftHeader[3], leftHeader[4] = byte(leftLength>>8), byte(leftLength)
	rightHeader := append([]byte(nil), packet[:5]...)
	rightHeader[3], rightHeader[4] = byte(rightLength>>8), byte(rightLength)
	var output bytes.Buffer
	output.Grow(len(packet) + 5)
	output.Write(leftHeader)
	output.Write(packet[5:splitAt])
	boundary := output.Len()
	output.Write(rightHeader)
	output.Write(packet[splitAt:recordEnd])
	output.Write(packet[recordEnd:])
	return output.Bytes(), boundary, true
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func readUint16(value []byte, position int) int {
	return int(value[position])<<8 | int(value[position+1])
}
