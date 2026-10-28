package datapath

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// A short-lived, exact-host route verified with TLS and a bounded HTTP body.
// Never applies to another host, a non-TLS flow, or a non-443 port. Original
// application TLS and its certificate checks remain completely untouched.
type verifiedServiceRoute struct {
	Address  string     `json:"address"`
	Strategy strategyID `json:"strategy"`
	Expires  time.Time  `json:"expires"`
	Failures int        `json:"failures"`
}

const verifiedRouteTTL = 15 * time.Minute

func serviceProbeHost(host string) bool {
	switch host {
	case "x.com", "api.x.com", "api.twitter.com", "pbs.twimg.com", "video.twimg.com":
		return true
	case "www.instagram.com", "www.facebook.com", "scontent-hel3-1.xx.fbcdn.net", "scontent-arn2-1.xx.fbcdn.net":
		return true
	case "chat.signal.org", "grpc.chat.signal.org", "api.whatsapp.net", "www.whatsapp.com", "mmg.whatsapp.net", "static.whatsapp.net", "www.youtube.com", "youtubei.googleapis.com", "youtube.googleapis.com":
		return true
	}
	return false
}

func (p *lightPolicy) verifiedRoute(host string, port uint16) (verifiedServiceRoute, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.verifiedServices[host]
	return r, ok && port == 443 && time.Now().Before(r.Expires) && r.Failures < 2
}

func (p *lightPolicy) reportVerifiedRoute(host, address string, success bool, expectedStrategy ...strategyID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.verifiedServices[host]
	if !ok || r.Address != address || (len(expectedStrategy) > 0 && r.Strategy != expectedStrategy[0]) {
		return
	}
	if success {
		r.Failures = 0
		// Keep an actively responding route alive. Idle routes still expire;
		// transport activity is not a claim that media playback succeeded.
		r.Expires = time.Now().Add(verifiedRouteTTL)
	} else {
		r.Failures++
	}
	p.verifiedServices[host] = r
}

func (p *lightPolicy) probeServiceRoute(host, caPEM string) string {
	result := map[string]any{"host": host, "verified": false}
	encode := func() string { b, _ := json.Marshal(result); return string(b) }
	if !serviceProbeHost(host) {
		result["stage"] = "host-not-allowed"
		return encode()
	}
	youtube := classifyDomain(host) == serviceYouTube
	maintained := youtube || host == "chat.signal.org" || host == "grpc.chat.signal.org" || host == "api.whatsapp.net" || classifyDomain(host) == serviceMeta || classifyDomain(host) == serviceTwitter
	if r, ok := p.verifiedRoute(host, 443); ok && (!maintained || time.Until(r.Expires) > verifiedRouteTTL-time.Minute) {
		result["verified"], result["route"] = true, r
		return encode()
	}
	config := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	if host == "grpc.chat.signal.org" {
		config.NextProtos = []string{"h2"}
	}
	if classifyDomain(host) == serviceTwitter {
		config.NextProtos = []string{"h2"}
	}
	if host == "chat.signal.org" || host == "grpc.chat.signal.org" {
		config.RootCAs = x509.NewCertPool()
		if !config.RootCAs.AppendCertsFromPEM([]byte(caPEM)) {
			result["stage"] = "missing-signal-ca"
			return encode()
		}
	}
	if maintained {
		// Refresh the exact previously authenticated endpoint before rotating
		// DNS or strategies. Failed maintenance never erases a still-live route.
		p.mu.RLock()
		previous, exists := p.verifiedServices[host]
		p.mu.RUnlock()
		if exists && previous.Address != "" {
			ctx, cancel := context.WithTimeout(p.mtproxies.ctx, 3*time.Second)
			stage, code, n, err := probeServiceHTTP(ctx, p.doh.dialer, previous.Address, host, previous.Strategy, config)
			cancel()
			if err == nil {
				p.reportVerifiedRoute(host, previous.Address, true, previous.Strategy)
				result["verified"], result["stage"], result["status"], result["bytes"] = true, stage, code, n
				result["route"], _ = p.verifiedRoute(host, 443)
				return encode()
			}
		}
	}
	addresses := encryptedLookup(p.doh, host)
	p.observeDNS(host, addresses)
	var public []net.IP
	for _, ip := range addresses {
		if publicMTProxyIP(ip) {
			public = append(public, ip)
		}
	}
	// Exact prior DNS endpoints observed with certificate-verified responses on
	// this carrier. Candidates only: revalidate hostname/TLS every time before
	// publishing a route. DNS rotation must not hide the last working endpoint.
	seed := ""
	if host == "api.whatsapp.net" {
		seed = "57.144.245.32"
	}
	if host == "grpc.chat.signal.org" {
		seed = "15.197.251.99"
	}
	if host == "www.instagram.com" {
		seed = "57.144.244.34"
	}
	if host == "www.facebook.com" {
		seed = "157.240.205.35"
	}
	if host == "x.com" || host == "api.x.com" || host == "api.twitter.com" {
		// Observed in api.twitter.com's Google DoH answer; each exact SNI
		// separately passed verified TLS/HTTP2 on this LTE network.
		seed = "151.101.194.146"
	}
	if host == "pbs.twimg.com" {
		seed = "199.232.172.159"
	}
	if host == "video.twimg.com" {
		seed = "199.232.172.158"
	}
	if seed != "" {
		candidate := net.ParseIP(seed)
		filtered := []net.IP{candidate}
		for _, ip := range public {
			if !ip.Equal(candidate) {
				filtered = append(filtered, ip)
			}
		}
		public = filtered
	}
	if len(public) == 0 {
		result["stage"] = "no-public-ipv4"
		return encode()
	}
	strategies := []strategyID{strategyDirect, strategyTLSRecord, strategyComboFast, strategyOOB, strategyComboMid, strategyTLSCombo}
	if youtube {
		// Go and Chromium ClientHello shapes are filtered differently on LTE.
		// Probe the actual browser family before publishing an exact-host route.
		strategies = []strategyID{strategyComboFast, strategyComboMid, strategyTLSCombo, strategyTLSRecord, strategyDirect, strategyOOB}
	}
	if classifyDomain(host) == serviceTwitter {
		strategies = []strategyID{strategyComboFast, strategyDisorder, strategyDirect}
	}
	if classifyDomain(host) == serviceMeta {
		strategies = []strategyID{strategyDisorder, strategyComboFast, strategyTLSRecord, strategyDirect}
	}
	if classifyDomain(host) == serviceWhatsApp {
		strategies = []strategyID{strategyDirect, strategyOOB, strategyTLSRecord, strategyComboFast}
	}
	if host == "api.whatsapp.net" {
		strategies = []strategyID{strategyOOB, strategyDirect, strategyTLSRecord, strategyComboFast}
	}
	if host == "grpc.chat.signal.org" {
		strategies = []strategyID{strategyTLSRecord, strategyOOB, strategyDirect, strategyComboFast}
	}
	ctx, cancel := context.WithTimeout(p.mtproxies.ctx, 18*time.Second)
	defer cancel()
	for _, strategy := range strategies {
		for _, ip := range public[:min(2, len(public))] {
			if ctx.Err() != nil {
				result["stage"] = "deadline"
				return encode()
			}
			address := net.JoinHostPort(ip.String(), "443")
			attempt, stop := context.WithTimeout(ctx, 3*time.Second)
			stage, code, n, err := probeServiceHTTP(attempt, p.doh.dialer, address, host, strategy, config)
			stop()
			result["stage"], result["address"], result["strategy"] = stage, address, strategy
			if err != nil {
				result["error"] = err.Error()
				continue
			}
			r := verifiedServiceRoute{Address: address, Strategy: strategy, Expires: time.Now().Add(verifiedRouteTTL)}
			p.mu.Lock()
			if p.verifiedServices == nil {
				p.verifiedServices = map[string]verifiedServiceRoute{}
			}
			p.verifiedServices[host] = r
			p.mu.Unlock()
			delete(result, "error")
			result["verified"], result["route"], result["status"], result["bytes"] = true, r, code, n
			return encode()
		}
	}
	return encode()
}

func probeServiceHTTP(ctx context.Context, dialer *net.Dialer, address, host string, strategy strategyID, config *tls.Config) (string, int, int64, error) {
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return "TCP", 0, 0, err
	}
	defer raw.Close()
	deadline, _ := ctx.Deadline()
	raw.SetDeadline(deadline)
	var conn net.Conn
	var negotiated string
	if classifyDomain(host) == serviceYouTube {
		browser := utls.UClient(&firstWriteStrategyConn{Conn: raw, strategy: strategy}, &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: config.RootCAs}, utls.HelloChrome_Auto)
		if err := browser.HandshakeContext(ctx); err != nil {
			return "TLS-Chromium", 0, 0, err
		}
		conn, negotiated = browser, browser.ConnectionState().NegotiatedProtocol
	} else {
		standard := tls.Client(&firstWriteStrategyConn{Conn: raw, strategy: strategy}, config)
		if err := standard.HandshakeContext(ctx); err != nil {
			return "TLS", 0, 0, err
		}
		conn, negotiated = standard, standard.ConnectionState().NegotiatedProtocol
	}
	req, _ := http.NewRequest("GET", "https://"+host+"/robots.txt", nil)
	if host == "api.x.com" || host == "api.twitter.com" {
		req, _ = http.NewRequest("GET", "https://"+host+"/2/users/me", nil)
	}
	req.Header.Set("Connection", "close")
	req.Header.Set("User-Agent", "Argus-network-diagnostic/1")
	req.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
	req.Header.Set("Pragma", "no-cache")
	if host == "grpc.chat.signal.org" || negotiated == "h2" {
		// Only endpoint reachability: HTTP/2 headers are not gRPC messaging or
		// call success. A rejected anonymous GET may leave its body open.
		if negotiated != "h2" {
			return "HTTP2-ALPN", 0, 0, fmt.Errorf("HTTP2 not negotiated")
		}
		transport := &http2.Transport{}
		cc, err := transport.NewClientConn(conn)
		if err != nil {
			return "HTTP2-setup", 0, 0, err
		}
		defer cc.Close()
		req.Header.Del("Connection")
		res, err := cc.RoundTrip(req.WithContext(ctx))
		if err != nil {
			return "HTTP2-headers", 0, 0, err
		}
		if classifyDomain(host) == serviceYouTube {
			defer res.Body.Close()
			n, err := io.Copy(io.Discard, io.LimitReader(res.Body, 32768))
			if err == nil && (n == 0 || (res.StatusCode != 200 && res.StatusCode != 404 && res.StatusCode != 405)) {
				err = fmt.Errorf("YouTube endpoint HTTP=%d body=%d", res.StatusCode, n)
			}
			return "HTTP2-Chromium", res.StatusCode, n, err
		}
		res.Body.Close()
		if classifyDomain(host) == serviceTwitter && res.StatusCode != 200 && res.StatusCode != 400 && res.StatusCode != 401 {
			return "HTTP2-rejected", res.StatusCode, 0, fmt.Errorf("Twitter probe rejected: %d", res.StatusCode)
		}
		return "HTTP2-headers-only", res.StatusCode, 0, nil
	}
	if err := req.Write(conn); err != nil {
		return "HTTP-write", 0, 0, err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return "HTTP-read", 0, 0, err
	}
	defer res.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, 32768))
	if err == nil && n == 0 {
		err = fmt.Errorf("empty HTTP body")
	}
	return "HTTP", res.StatusCode, n, err
}

func normalizeProbeHost(host string) string { return strings.TrimSuffix(strings.ToLower(host), ".") }
