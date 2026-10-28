package datapath

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// Explicit opt-in only. The application's inner TLS stream is not terminated,
// rewritten or inspected beyond its SNI. Certificate verification stays enabled.
func signalRelayHost(host string, port uint16) bool {
	return port == 443 && (host == "chat.signal.org" || host == "grpc.chat.signal.org")
}

func signalDirectTrialHost(host string, port uint16) bool {
	return host == "grpc.chat.signal.org" && port == 443
}

func dialSignalRelay(ctx context.Context, dialer *net.Dialer) (net.Conn, error) {
	// Public operator consented to for this alpha. No automatic relay discovery.
	raw, err := dialer.DialContext(ctx, "tcp", "104.248.203.186:443")
	if err != nil {
		return nil, err
	}
	outer := tls.Client(raw, &tls.Config{
		ServerName: "sgpx.toomore.net",
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	})
	if err := outer.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	_ = outer.SetDeadline(time.Time{})
	return outer, nil
}
