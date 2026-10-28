package datapath

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type dohEndpoint struct {
	name   string
	client *http.Client
}

type dotEndpoint struct {
	name    string
	address string
}

type dohResolver struct {
	endpoints      []dohEndpoint
	gemini         *dohResolver
	auditMu        sync.Mutex
	googleQueries  map[string]int
	geminiAttempts atomic.Int64
	geminiSuccess  atomic.Int64
	dot            []dotEndpoint
	dialer         *net.Dialer
	observe        func(string, []net.IP)
	healthy        atomic.Bool
	attempts       atomic.Int64
	successes      atomic.Int64
	transport      atomic.Value
	preferred      atomic.Int32
	backoff        atomic.Int64
}

const (
	dnsIdleTimeout      = 75 * time.Second
	dnsEncryptedTimeout = 2 * time.Second
	dnsRaceTimeout      = 3 * time.Second
	dnsPlaintextTimeout = 2 * time.Second
	dnsEncryptedBackoff = 60 * time.Second
)

func newDoHResolver(dialer *net.Dialer, observe func(string, []net.IP)) *dohResolver {
	resolver := &dohResolver{
		dialer:  dialer,
		observe: observe,
		endpoints: []dohEndpoint{
			newDoHEndpoint(dialer, "cloudflare-dns.com", "1.1.1.1:443"),
			newDoHEndpoint(dialer, "cloudflare-dns.com", "[2606:4700:4700::1111]:443"),
			newDoHEndpoint(dialer, "dns.google", "8.8.8.8:443"),
			newDoHEndpoint(dialer, "dns.google", "[2001:4860:4860::8888]:443"),
			newDoHEndpoint(dialer, "dns.quad9.net", "9.9.9.9:443"),
			newDoHEndpoint(dialer, "dns.quad9.net", "[2620:fe::fe]:443"),
			newDoHEndpoint(dialer, "dns.adguard-dns.com", "94.140.14.140:443"),
			newDoHEndpoint(dialer, "dns.adguard-dns.com", "[2a10:50c0::ad1:ff]:443"),
		},
		dot: []dotEndpoint{
			{name: "cloudflare-dns.com", address: "1.1.1.1:853"},
			{name: "cloudflare-dns.com", address: "[2606:4700:4700::1111]:853"},
			{name: "dns.google", address: "8.8.8.8:853"},
			{name: "dns.google", address: "[2001:4860:4860::8888]:853"},
			{name: "dns.quad9.net", address: "9.9.9.9:853"},
			{name: "dns.quad9.net", address: "[2620:fe::fe]:853"},
			{name: "dns.adguard-dns.com", address: "94.140.14.140:853"},
			{name: "dns.adguard-dns.com", address: "[2a10:50c0::ad1:ff]:853"},
		},
	}
	resolver.transport.Store("CHECK")
	// Separate pool: COMSS must never win the general resolver race.
	resolver.gemini = &dohResolver{endpoints: []dohEndpoint{
		newDoHEndpoint(dialer, "dns.comss.one", "195.133.25.16:443"),
	}}
	return resolver
}

func newDoHEndpoint(dialer *net.Dialer, serverName, address string) dohEndpoint {
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		},
	}
	return dohEndpoint{
		name:   serverName,
		client: &http.Client{Transport: transport, Timeout: 8 * time.Second},
	}
}

func (r *dohResolver) serve(client net.Conn) {
	defer client.Close()
	buffer := make([]byte, 65535)
	for {
		_ = client.SetReadDeadline(time.Now().Add(dnsIdleTimeout))
		count, err := client.Read(buffer)
		if count > 0 {
			response, resolveErr := r.resolve(append([]byte(nil), buffer[:count]...))
			if resolveErr == nil {
				r.healthy.Store(true)
				if domain, addresses, parseErr := parseDNSAnswer(response); parseErr == nil {
					r.observe(domain, addresses)
				}
				_, _ = client.Write(response)
			} else {
				r.healthy.Store(false)
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *dohResolver) resolve(query []byte) ([]byte, error) {
	r.attempts.Add(1)
	if domain, _, err := parseDNSAnswer(query); err == nil {
		if strings.HasSuffix(domain, ".googleapis.com") || strings.HasSuffix(domain, ".google.com") || strings.HasSuffix(domain, ".clients6.google.com") {
			r.auditMu.Lock()
			if r.googleQueries == nil {
				r.googleQueries = make(map[string]int)
			}
			if len(r.googleQueries) < 64 || r.googleQueries[domain] > 0 {
				r.googleQueries[domain]++
			}
			r.auditMu.Unlock()
		}
	}
	if domain, _, err := parseDNSAnswer(query); err == nil && isGeminiDNSName(domain) && r.gemini != nil {
		r.geminiAttempts.Add(1)
		if body, err := r.gemini.resolveDoH(0, query, dnsRaceTimeout); err == nil && body[3]&15 == 0 {
			r.geminiSuccess.Add(1)
			return r.acceptDNS(body, "DoH COMSS (Gemini)"), nil
		}
		// A provider failure must not disable ordinary DNS resolution.
	}
	preferred := int(r.preferred.Load())
	// A known-good encrypted resolver must be tried for every A/AAAA question.
	// The old gate let only one request in an Android DNS burst use DoH; all
	// concurrent questions silently fell through to carrier UDP DNS.
	if body, err := r.resolveDoH(preferred, query, dnsEncryptedTimeout); err == nil {
		return r.acceptDNS(body, "DoH"), nil
	}
	now := time.Now().UnixMilli()
	backoff := r.backoff.Load()
	// Only one concurrent DNS request probes the full encrypted endpoint set.
	// Android often emits a burst of parallel A/AAAA questions; allowing every
	// one to start sixteen blocked TLS handshakes caused the field-reported
	// resolver collapse and needlessly consumed radio time.
	probeUntil := time.Now().Add(dnsEncryptedBackoff).UnixMilli()
	if now >= backoff && r.backoff.CompareAndSwap(backoff, probeUntil) {
		if body, transport, index, err := r.raceEncrypted(query, preferred); err == nil {
			if transport == "DoH" {
				r.preferred.Store(int32(index))
			}
			return r.acceptDNS(body, transport), nil
		}
	}
	if body, err := r.raceUDP(query); err == nil {
		return r.acceptDNS(body, "UDP fallback"), nil
	}
	return nil, fmt.Errorf("encrypted and UDP DNS unavailable")
}

func (r *dohResolver) googleDNSDiagnostics() map[string]int {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	copy := make(map[string]int, len(r.googleQueries))
	for name, count := range r.googleQueries {
		copy[name] = count
	}
	return copy
}

func isGeminiDNSName(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(name, ".")) {
	case "gemini.google.com", "aistudio.google.com", "generativelanguage.googleapis.com", "alkalimakersuite-pa.clients6.google.com", "robinfrontend-pa.googleapis.com", "notebooklm-pa.googleapis.com", "notebooklm.google.com", "notebook.google.com":
		return true
	}
	return false
}

func (r *dohResolver) raceEncrypted(query []byte, skip int) ([]byte, string, int, error) {
	type result struct {
		body      []byte
		transport string
		index     int
		err       error
	}
	results := make(chan result, 2)
	go func() {
		body, index, err := r.raceDoH(query, skip)
		results <- result{body: body, transport: "DoH", index: index, err: err}
	}()
	go func() {
		body, err := r.raceDoT(query)
		results <- result{body: body, transport: "DoT", err: err}
	}()
	var last error
	for range 2 {
		result := <-results
		if result.err == nil {
			return result.body, result.transport, result.index, nil
		}
		last = result.err
	}
	return nil, "", 0, last
}

func (r *dohResolver) acceptDNS(body []byte, transport string) []byte {
	r.successes.Add(1)
	r.healthy.Store(true)
	r.transport.Store(transport)
	if transport != "UDP fallback" {
		r.backoff.Store(0)
	}
	return body
}

func (r *dohResolver) resolveDoH(index int, query []byte, timeout time.Duration) ([]byte, error) {
	if index < 0 || index >= len(r.endpoints) {
		index = 0
	}
	endpoint := r.endpoints[index]
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint.name+"/dns-query", bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/dns-message")
	request.Header.Set("Content-Type", "application/dns-message")
	response, err := endpoint.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	if err != nil || response.StatusCode != http.StatusOK || !validDNSResponse(query, body) {
		return nil, fmt.Errorf("%s returned unusable DNS response", endpoint.name)
	}
	return body, nil
}

func (r *dohResolver) raceDoH(query []byte, skip int) ([]byte, int, error) {
	type result struct {
		body  []byte
		index int
		err   error
	}
	results := make(chan result, len(r.endpoints))
	count := 0
	for index := range r.endpoints {
		if index == skip {
			continue
		}
		count++
		go func(index int) {
			body, err := r.resolveDoH(index, query, dnsRaceTimeout)
			results <- result{body, index, err}
		}(index)
	}
	var last error
	for range count {
		result := <-results
		if result.err == nil {
			return result.body, result.index, nil
		}
		last = result.err
	}
	return nil, 0, last
}

func (r *dohResolver) raceDoT(query []byte) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	results := make(chan result, len(r.dot))
	for _, endpoint := range r.dot {
		go func(endpoint dotEndpoint) {
			body, err := r.resolveDoT(endpoint, query)
			results <- result{body, err}
		}(endpoint)
	}
	var last error
	for range r.dot {
		result := <-results
		if result.err == nil {
			return result.body, nil
		}
		last = result.err
	}
	return nil, last
}

func (r *dohResolver) raceUDP(query []byte) ([]byte, error) {
	addresses := []string{
		"1.1.1.1:53", "[2606:4700:4700::1111]:53",
		"8.8.8.8:53", "[2001:4860:4860::8888]:53",
		"9.9.9.9:53", "[2620:fe::fe]:53",
		"94.140.14.140:53", "[2a10:50c0::ad1:ff]:53",
	}
	type result struct {
		body []byte
		err  error
	}
	results := make(chan result, len(addresses))
	for _, address := range addresses {
		go func(address string) {
			ctx, cancel := context.WithTimeout(context.Background(), dnsPlaintextTimeout)
			defer cancel()
			connection, err := r.dialer.DialContext(ctx, "udp", address)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(dnsPlaintextTimeout))
			if _, err = connection.Write(query); err != nil {
				results <- result{err: err}
				return
			}
			body := make([]byte, 65535)
			count, err := connection.Read(body)
			body = body[:count]
			if err == nil && !validDNSResponse(query, body) {
				err = fmt.Errorf("invalid UDP DNS response")
			}
			results <- result{body, err}
		}(address)
	}
	var last error
	for range addresses {
		result := <-results
		if result.err == nil {
			return result.body, nil
		}
		last = result.err
	}
	return nil, last
}

func validDNSResponse(query, response []byte) bool {
	return len(query) >= 2 && len(response) >= 12 &&
		query[0] == response[0] && query[1] == response[1] && response[2]&0x80 != 0
}

func (r *dohResolver) resolveDoT(endpoint dotEndpoint, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsRaceTimeout)
	defer cancel()
	raw, err := r.dialer.DialContext(ctx, "tcp", endpoint.address)
	if err != nil {
		return nil, fmt.Errorf("%s DoT dial: %w", endpoint.name, err)
	}
	defer raw.Close()
	connection := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: endpoint.name,
	})
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(dnsRaceTimeout))
	if err := connection.Handshake(); err != nil {
		return nil, fmt.Errorf("%s DoT handshake: %w", endpoint.name, err)
	}
	framed := make([]byte, len(query)+2)
	binary.BigEndian.PutUint16(framed, uint16(len(query)))
	copy(framed[2:], query)
	if _, err := connection.Write(framed); err != nil {
		return nil, fmt.Errorf("%s DoT write: %w", endpoint.name, err)
	}
	var size [2]byte
	if _, err := io.ReadFull(connection, size[:]); err != nil {
		return nil, fmt.Errorf("%s DoT length: %w", endpoint.name, err)
	}
	length := int(binary.BigEndian.Uint16(size[:]))
	if length < 12 {
		return nil, fmt.Errorf("%s returned short DoT response", endpoint.name)
	}
	response := make([]byte, length)
	if _, err := io.ReadFull(connection, response); err != nil {
		return nil, fmt.Errorf("%s DoT response: %w", endpoint.name, err)
	}
	return response, nil
}

func (r *dohResolver) activeTransport() string {
	value, _ := r.transport.Load().(string)
	return value
}

func parseDNSAnswer(message []byte) (string, []net.IP, error) {
	if len(message) < 12 {
		return "", nil, fmt.Errorf("short DNS response")
	}
	questions := readUint16(message, 4)
	answers := readUint16(message, 6)
	position := 12
	domain := ""
	for index := 0; index < questions; index++ {
		name, next, err := readDNSName(message, position, 0)
		if err != nil || next+4 > len(message) {
			return "", nil, fmt.Errorf("invalid DNS question")
		}
		if index == 0 {
			domain = name
		}
		position = next + 4
	}
	addresses := make([]net.IP, 0, answers)
	for index := 0; index < answers; index++ {
		_, next, err := readDNSName(message, position, 0)
		if err != nil || next+10 > len(message) {
			return domain, addresses, fmt.Errorf("invalid DNS answer")
		}
		typeID := readUint16(message, next)
		length := readUint16(message, next+8)
		data := next + 10
		if data+length > len(message) {
			return domain, addresses, fmt.Errorf("invalid DNS record length")
		}
		if typeID == 1 && length == net.IPv4len {
			addresses = append(addresses, append(net.IP(nil), message[data:data+length]...))
		} else if typeID == 28 && length == net.IPv6len {
			addresses = append(addresses, append(net.IP(nil), message[data:data+length]...))
		}
		position = data + length
	}
	return domain, addresses, nil
}

func readDNSName(message []byte, position, depth int) (string, int, error) {
	if depth > 12 || position >= len(message) {
		return "", 0, fmt.Errorf("invalid DNS name")
	}
	labels := make([]string, 0, 4)
	next := position
	for {
		if position >= len(message) {
			return "", 0, fmt.Errorf("truncated DNS name")
		}
		length := int(message[position])
		if length&0xc0 == 0xc0 {
			if position+1 >= len(message) {
				return "", 0, fmt.Errorf("truncated DNS pointer")
			}
			pointer := (length&0x3f)<<8 | int(message[position+1])
			suffix, _, err := readDNSName(message, pointer, depth+1)
			if err != nil {
				return "", 0, err
			}
			if suffix != "" {
				labels = append(labels, suffix)
			}
			next = position + 2
			break
		}
		position++
		if length == 0 {
			next = position
			break
		}
		if length > 63 || position+length > len(message) {
			return "", 0, fmt.Errorf("invalid DNS label")
		}
		labels = append(labels, string(message[position:position+length]))
		position += length
		next = position
	}
	return strings.Join(labels, "."), next, nil
}
