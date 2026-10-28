package datapath

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/mtproxy"
)

type mtProxyConfig struct {
	host, port, id string
	secret         mtproxy.Secret
	wsDC           int
}

func parseMTProxy(value string) (mtProxyConfig, bool) {
	u, err := url.Parse(value)
	if err != nil {
		return mtProxyConfig{}, false
	}
	if u.Scheme == "wss" {
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/apiws" || u.RawPath != "" || (u.Port() != "" && u.Port() != "443") {
			return mtProxyConfig{}, false
		}
		host := strings.ToLower(u.Hostname())
		for dc := 1; dc <= 5; dc++ {
			for _, domain := range []string{"pclead.co.uk", "cakeisalie.co.uk"} {
				if host == fmt.Sprintf("kws%d.%s", dc, domain) {
					sum := sha256.Sum256([]byte("wss://" + host + "/apiws"))
					return mtProxyConfig{host: host, port: "443", id: hex.EncodeToString(sum[:4]), wsDC: dc}, true
				}
			}
		}
		return mtProxyConfig{}, false
	}
	if !(u.Scheme == "tg" && u.Host == "proxy" || u.Scheme == "https" && u.Host == "t.me" && u.Path == "/proxy") {
		return mtProxyConfig{}, false
	}
	q := u.Query()
	host := strings.TrimSuffix(strings.ToLower(q.Get("server")), ".")
	port := q.Get("port")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, "/\\ @[]:\x00\t\r\n") {
		return mtProxyConfig{}, false
	}
	if ip := net.ParseIP(host); ip != nil && !publicMTProxyIP(ip) {
		return mtProxyConfig{}, false
	}
	raw := q.Get("secret")
	b, err := hex.DecodeString(raw)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	}
	if err != nil {
		return mtProxyConfig{}, false
	}
	s, err := mtproxy.ParseSecret(b)
	// Alpha transport supports genuine EE FakeTLS only, not mislabeled DD blobs.
	if err != nil || s.Type != mtproxy.TLS || s.Tag != 0xee || len(s.CloakHost) == 0 || len(s.CloakHost) > 253 || strings.ContainsAny(s.CloakHost, "/\\ @\x00\t\r\n") {
		return mtProxyConfig{}, false
	}
	sum := sha256.Sum256([]byte(net.JoinHostPort(host, port) + "/" + hex.EncodeToString(b)))
	return mtProxyConfig{host: host, port: port, id: hex.EncodeToString(sum[:4]), secret: s}, true
}
func publicMTProxyIP(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] != 0 && v4[0] < 224 && !(v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

type mtProxyHealth struct {
	verified, nextTry time.Time
	failures          int
	latency           time.Duration
	address           string
}
type mtProxyPool struct {
	traceSequence    uint64
	connectionTraces []connectionTrace
	mu               sync.Mutex
	verifyMu         [10]sync.Mutex // main/media DC1..5; a dead DC must not stall all others
	configs          []mtProxyConfig
	health           map[string]mtProxyHealth
	auto, searching  bool
	nextSearch       [10]time.Time
	searchFailures   [10]int
	searchCursor     int
	searchDC         int
	lastStage        string
	active           string
	flows, received  int64
	unmapped         map[string]int
	transportEvents  map[string]int64
	unrecognized     int
	ctx              context.Context
	cancel           context.CancelFunc
}

func newMTProxyPool() *mtProxyPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &mtProxyPool{health: map[string]mtProxyHealth{}, ctx: ctx, cancel: cancel}
}
func (p *mtProxyPool) configure(encoded string, auto bool) {
	var configs []mtProxyConfig
	seen := map[string]bool{}
	for _, line := range strings.Fields(encoded) {
		c, ok := parseMTProxy(line)
		if !ok || seen[c.id] {
			continue
		}
		seen[c.id] = true
		configs = append(configs, c)
		if len(configs) == 12 {
			break
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configs = configs
	p.auto = auto
	p.health = map[string]mtProxyHealth{}
	p.nextSearch = [10]time.Time{}
	p.searchFailures = [10]int{}
	p.searchCursor = 0
	p.active = ""
	p.lastStage = "configured"
}
func (p *mtProxyPool) enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.configs) > 0 || p.auto
}
func (p *mtProxyPool) statusJSON() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, _ := json.Marshal(struct {
		Count           int               `json:"count"`
		Auto            bool              `json:"auto"`
		Searching       bool              `json:"searching"`
		Active          string            `json:"active,omitempty"`
		Stage           string            `json:"stage,omitempty"`
		Flows           int64             `json:"flows"`
		Received        int64             `json:"received"`
		Unmapped        map[string]int    `json:"unmappedDc,omitempty"`
		Unrecognized    int               `json:"unsupportedHeaders"`
		TransportEvents map[string]int64  `json:"transportEvents,omitempty"`
		VerifiedMainDCs []int             `json:"verifiedMainDcs"`
		SearchDC        int               `json:"searchDc,omitempty"`
		Connections     []connectionTrace `json:"connections,omitempty"`
	}{len(p.configs), p.auto, p.searching, p.active, p.lastStage, p.flows, p.received, p.unmapped, p.unrecognized, p.transportEvents, p.verifiedMainDCsLocked(time.Now()), p.searchDC, p.connectionTraces})
	return b
}

// Test evidence, not account delivery. A discovery failure or another DC's
// failure cannot erase still-fresh nonce evidence for an independent DC.
func (p *mtProxyPool) verifiedMainDCsLocked(now time.Time) []int {
	result := []int{}
	for dc := 1; dc <= 5; dc++ {
		for key, h := range p.health {
			if strings.HasSuffix(key, fmt.Sprintf("/%d", dc)) &&
				!h.verified.IsZero() && now.Sub(h.verified) <= 2*time.Minute &&
				!now.Before(h.nextTry) {
				result = append(result, dc)
				break
			}
		}
	}
	return result
}

// Bounded counters only: no packet bodies, account identifiers or keys.
func (p *mtProxyPool) transportEvent(key string, count int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.transportEvents == nil {
		p.transportEvents = map[string]int64{}
	}
	if _, ok := p.transportEvents[key]; ok || len(p.transportEvents) < 128 {
		p.transportEvents[key] += count
	}
}
func (p *mtProxyPool) report(id string, dc int, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprintf("%s/%d", id, dc)
	h := p.health[key]
	if success {
		h.failures = 0
		h.nextTry = time.Time{}
	} else {
		h.failures++
		delay := 30 * time.Second * time.Duration(1<<min(h.failures-1, 4))
		h.nextTry = time.Now().Add(delay)
		h.verified = time.Time{}
		p.lastStage = "route-failed"
		if p.active == id {
			p.active = ""
		}
	}
	p.health[key] = h
}
func (p *mtProxyPool) resolve(c mtProxyConfig, resolver *dohResolver) (string, error) {
	ips := []net.IP{net.ParseIP(c.host)}
	if ips[0] == nil {
		ips = encryptedLookup(resolver, c.host)
	}
	for _, ip := range ips {
		if publicMTProxyIP(ip) {
			return net.JoinHostPort(ip.String(), c.port), nil
		}
	}
	return "", fmt.Errorf("no public IPv4 route to MTProxy")
}
func (p *mtProxyPool) dial(dialer *net.Dialer, resolver *dohResolver, dc int) (net.Conn, string, error) {
	dcSlot := dc - 1
	if dc < 0 {
		dcSlot = -dc + 4
	}
	if dcSlot < 0 || dcSlot >= len(p.verifyMu) {
		return nil, "", fmt.Errorf("unsupported MTProxy DC")
	}
	p.mu.Lock()
	configs := append([]mtProxyConfig(nil), p.configs...)
	health := map[string]mtProxyHealth{}
	for k, v := range p.health {
		health[k] = v
	}
	p.mu.Unlock()
	sort.SliceStable(configs, func(i, j int) bool {
		a, b := health[fmt.Sprintf("%s/%d", configs[i].id, dc)], health[fmt.Sprintf("%s/%d", configs[j].id, dc)]
		return a.verified.After(b.verified)
	})
	var last error = fmt.Errorf("no available verified MTProxy")
	attempts := 0
	for _, c := range configs {
		if c.wsDC != 0 && c.wsDC != dc && c.wsDC != -dc {
			continue
		}
		key := fmt.Sprintf("%s/%d", c.id, dc)
		if time.Now().Before(health[key].nextTry) {
			continue
		}
		attempts++
		if attempts > 3 {
			break
		}
		// A rotating public hostname can return a different (dead) backend on
		// each DNS query. Reverify the same nonce-verified endpoint; after
		// failure, report clears verified and the next attempt resolves afresh.
		address := mtProxyReusableAddress(health[key], time.Now())
		var err error
		if address == "" {
			address, err = p.resolve(c, resolver)
		}
		if err != nil {
			last = err
			p.report(c.id, dc, false)
			continue
		}
		// Serialize verification only; established streams are never serialized.
		p.verifyMu[dcSlot].Lock()
		p.mu.Lock()
		h := p.health[key]
		p.mu.Unlock()
		if time.Now().Before(h.nextTry) {
			p.verifyMu[dcSlot].Unlock()
			last = fmt.Errorf("MTProxy DC in cooldown")
			continue
		}
		if recent := mtProxyReusableAddress(h, time.Now()); recent != "" {
			address = recent
		}
		if time.Since(h.verified) > 2*time.Minute || h.address != address {
			ctx, cancel := context.WithTimeout(p.ctx, 7*time.Second)
			start := time.Now()
			probe, e := openConfiguredMTProxy(ctx, dialer, address, c, dc)
			if e == nil {
				e = verifyMTProxyConnection(probe)
				probe.Close()
			}
			cancel()
			err = e
			if e == nil {
				p.mu.Lock()
				p.health[key] = mtProxyHealth{verified: time.Now(), latency: time.Since(start), address: address}
				p.lastStage = "nonce-verified"
				p.mu.Unlock()
			}
		}
		p.verifyMu[dcSlot].Unlock()
		if err != nil {
			last = err
			p.report(c.id, dc, false)
			continue
		}
		ctx, cancel := context.WithTimeout(p.ctx, 7*time.Second)
		conn, err := openConfiguredMTProxy(ctx, dialer, address, c, dc)
		cancel()
		if err != nil {
			last = err
			p.report(c.id, dc, false)
			continue
		}
		p.mu.Lock()
		p.active = c.id
		p.lastStage = "connected"
		p.flows++
		p.mu.Unlock()
		return conn, c.id, nil
	}
	p.discoverForDC(dialer, resolver, dc)
	return nil, "", last
}

func mtProxyReusableAddress(h mtProxyHealth, now time.Time) string {
	// Expired VERIFICATION requires rechecking this endpoint, not throwing its
	// address away and choosing a random DNS backend. dial() still performs a
	// fresh nonce check after two minutes; failures invalidate the address.
	if h.address != "" && !h.verified.IsZero() && !now.Before(h.nextTry) {
		return h.address
	}
	return ""
}

// A failed local TUN connection otherwise closes instantly during proxy
// cooldown, causing thousands of reconnects without any upstream attempt.
func (p *mtProxyPool) waitAfterDialFailure(start time.Time) {
	delay := 2*time.Second - time.Since(start)
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-p.ctx.Done():
	}
}

// Download plain public link data only (never code). This network activity is
// disabled by default and requires explicit opt-in in Argus's proxy settings.
func (p *mtProxyPool) discover(dialer *net.Dialer, resolver *dohResolver) {
	p.discoverForDC(dialer, resolver, 2)
}

func (p *mtProxyPool) discoverForDC(dialer *net.Dialer, resolver *dohResolver, dc int) {
	if dc == 0 || dc < -5 || dc > 5 {
		return
	}
	slot := telegramSearchSlot(dc)
	p.mu.Lock()
	if !p.auto || p.searching || time.Now().Before(p.nextSearch[slot]) || p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.searching = true
	p.searchDC = dc
	p.mu.Unlock()
	go func() {
		found := false
		defer func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.searching = false
			if found {
				p.searchFailures[slot] = 0
				p.nextSearch[slot] = time.Now().Add(time.Minute)
			} else {
				p.searchFailures[slot]++
				p.nextSearch[slot] = time.Now().Add(time.Minute * time.Duration(1<<min(p.searchFailures[slot]-1, 4)))
				p.lastStage = "search-exhausted"
			}
		}()
		ctx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
		defer cancel()
		configs, err := fetchMTProxyCandidates(ctx, dialer, resolver)
		if err != nil {
			p.mu.Lock()
			p.lastStage = "list-unavailable"
			p.mu.Unlock()
			return
		}
		// Rotate bounded batches, rather than retrying the same dead prefix.
		p.mu.Lock()
		configs, p.searchCursor = mtProxyBatch(configs, p.searchCursor, 18)
		p.mu.Unlock()
		var wg sync.WaitGroup
		limit := make(chan struct{}, 3)
		for _, c := range configs {
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(c mtProxyConfig) {
				defer wg.Done()
				defer func() { <-limit }()
				address, err := p.resolve(c, resolver)
				if err != nil {
					return
				}
				probeCtx, stop := context.WithTimeout(ctx, 7*time.Second)
				defer stop()
				start := time.Now()
				conn, err := openMTProxy(probeCtx, dialer, address, c.secret, dc)
				if err != nil {
					return
				}
				err = verifyMTProxyConnection(conn)
				conn.Close()
				if err != nil {
					return
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				found = p.recordDiscoveredLocked(c, dc, address, time.Since(start)) || found
			}(c)
		}
		wg.Wait()
	}()
}

// Independent recovery windows for main and media on each DC. One bounded
// search still runs at a time, but its cooldown cannot suppress another DC.
func telegramSearchSlot(dc int) int {
	if dc < 0 {
		return -dc + 4
	}
	return dc - 1
}

// Caller holds mu. Keep evidence isolated by main/media DC; do not report an
// unusable candidate as found when the bounded configuration pool is full.
func (p *mtProxyPool) recordDiscoveredLocked(c mtProxyConfig, dc int, address string, latency time.Duration) bool {
	exists := false
	for _, old := range p.configs {
		if old.id == c.id {
			exists = true
			break
		}
	}
	if !exists {
		if len(p.configs) >= 12 {
			return false
		}
		p.configs = append(p.configs, c)
	}
	p.health[fmt.Sprintf("%s/%d", c.id, dc)] = mtProxyHealth{verified: time.Now(), latency: latency, address: address}
	p.lastStage = "alternative-verified"
	return true
}

func mtProxyBatch(configs []mtProxyConfig, cursor, size int) ([]mtProxyConfig, int) {
	if len(configs) == 0 || size <= 0 {
		return nil, 0
	}
	cursor = max(cursor, 0) % len(configs)
	size = min(size, len(configs))
	batch := make([]mtProxyConfig, size)
	for i := range batch {
		batch[i] = configs[(cursor+i)%len(configs)]
	}
	return batch, (cursor + size) % len(configs)
}

func fetchMTProxyCandidates(ctx context.Context, dialer *net.Dialer, resolver *dohResolver) ([]mtProxyConfig, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, _ := net.SplitHostPort(address)
		for _, ip := range encryptedLookup(resolver, host) {
			if publicMTProxyIP(ip) {
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
		}
		return nil, fmt.Errorf("list DNS unavailable")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var all []mtProxyConfig
	seen := map[string]bool{}
	var lastError error
	for _, source := range []string{
		"https://raw.githubusercontent.com/SoliSpirit/mtproto/master/all_proxies.txt",
		"https://raw.githubusercontent.com/aviamastersgh/mtproto-free-russia/main/verified_proxies.txt",
	} {
		batch, err := fetchMTProxyList(ctx, client, source)
		if err != nil {
			lastError = err
			continue
		}
		for _, c := range batch {
			if !seen[c.id] {
				all = append(all, c)
				seen[c.id] = true
			}
		}
	}
	if len(all) == 0 && lastError != nil {
		return nil, lastError
	}
	return all, nil
}

func fetchMTProxyList(ctx context.Context, client *http.Client, source string) ([]mtProxyConfig, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("list HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return nil, fmt.Errorf("list read/size error")
	}
	lines := strings.Split(string(data), "\n")
	var configs []mtProxyConfig
	seen := map[string]bool{}
	for i := len(lines) - 1; i >= 0; i-- {
		c, ok := parseMTProxy(strings.TrimSpace(lines[i]))
		if ok && !seen[c.id] {
			configs = append(configs, c)
			seen[c.id] = true
		}
	}
	return configs, nil
}
