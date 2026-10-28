package datapath

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/coder/websocket"
)

// Signal's own TLS-over-WebSocket reflector protocol. The original app TLS
// bytes remain opaque; only its destination SNI becomes X-Signal-Host.
// Configuration source: signalapp/libsignal rust/net/src/env.rs (production).
type signalReflectorEndpoint struct{ front, host string }

var signalReflectors = []signalReflectorEndpoint{
	{"www.google.com", "reflector-nrgwuv7kwq-uc.a.run.app"},
	{"github.githubassets.com", "reflector-signal.global.ssl.fastly.net"},
}

func signalReflectorTargetHost(host string, port uint16) bool {
	if port != 443 {
		return false
	}
	switch host {
	case "chat.signal.org", "grpc.chat.signal.org", "storage.signal.org", "cdn.signal.org", "cdn2.signal.org", "cdn3.signal.org":
		return true
	}
	return false
}

func dialSignalReflector(ctx, lifetime context.Context, dialer *net.Dialer, resolver *dohResolver, target string) (net.Conn, string, error) {
	if !signalReflectorTargetHost(target, 443) {
		return nil, "", fmt.Errorf("Signal reflector target not allowed")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn  net.Conn
		route string
		err   error
	}
	results := make(chan result)
	for _, endpoint := range signalReflectors {
		go func() {
			conn, err := openSignalReflector(ctx, lifetime, dialer, resolver, endpoint, target)
			select {
			case results <- result{conn, "signal-official-reflector/" + endpoint.front, err}:
			case <-ctx.Done():
				if conn != nil {
					conn.Close()
				}
			}
		}()
	}
	var last error
	for range signalReflectors {
		select {
		case candidate := <-results:
			if candidate.err == nil {
				return candidate.conn, candidate.route, nil
			}
			last = candidate.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return nil, "", last
}

func openSignalReflector(ctx, lifetime context.Context, dialer *net.Dialer, resolver *dohResolver, endpoint signalReflectorEndpoint, target string) (net.Conn, error) {
	var address string
	for _, ip := range encryptedLookup(resolver, endpoint.front) {
		if publicMTProxyIP(ip) {
			address = net.JoinHostPort(ip.String(), "443")
			break
		}
	}
	if address == "" {
		return nil, fmt.Errorf("Signal reflector has no public IPv4")
	}
	tr := &http.Transport{ForceAttemptHTTP2: false, DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		raw, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		outer := tls.Client(&firstWriteStrategyConn{Conn: raw, strategy: strategyComboFast}, &tls.Config{ServerName: endpoint.front, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		if err := outer.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return outer, nil
	}}
	defer tr.CloseIdleConnections()
	return upgradeSignalReflector(ctx, lifetime, &http.Client{Transport: tr}, "wss://"+endpoint.host+"/tls-tunnel", target)
}

func upgradeSignalReflector(ctx, lifetime context.Context, client *http.Client, url, target string) (net.Conn, error) {
	if !signalReflectorTargetHost(target, 443) {
		return nil, fmt.Errorf("Signal reflector target not allowed")
	}
	ws, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"X-Signal-Host": []string{target}}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if res != nil {
			res.Body.Close()
		}
		return nil, err
	}
	// Dial cancellation must not kill an established app stream. The engine
	// lifetime and forwarding deadlines own the upgraded connection instead.
	return websocket.NetConn(lifetime, ws, websocket.MessageBinary), nil
}
