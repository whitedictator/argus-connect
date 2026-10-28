package datapath

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Experimental native Noise transport to WhatsApp's own authenticated web
// endpoint. No third-party proxy, account login or inner protocol rewriting.
func dialWhatsAppWeb(ctx context.Context, dialer *net.Dialer, routing []byte) (net.Conn, error) {
	raw, err := dialer.DialContext(ctx, "tcp", "57.144.245.32:443")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			raw.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	raw.SetDeadline(deadline)
	c := tls.Client(&firstWriteStrategyConn{Conn: raw, strategy: strategyOOB}, &tls.Config{ServerName: "web.whatsapp.com", MinVersion: tls.VersionTLS12})
	if err = c.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	req, _ := http.NewRequest("GET", "https://web.whatsapp.com/ws/chat", nil)
	if len(routing) > 0 {
		query := req.URL.Query()
		query.Set("ED", base64.RawURLEncoding.EncodeToString(routing))
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", encoded)
	req.Header.Set("Origin", "https://web.whatsapp.com")
	if err = req.Write(c); err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: c, N: 32768}
	reader := bufio.NewReader(limited)
	res, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}
	expected := sha1.Sum([]byte(encoded + telegramWebSocketGUID))
	if res.StatusCode != 101 || !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") || res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(expected[:]) {
		res.Body.Close()
		return nil, fmt.Errorf("WhatsApp WebSocket upgrade rejected: %d", res.StatusCode)
	}
	// Preserve buffered bytes but remove the handshake-only read budget.
	limited.N = 1<<63 - 1
	c.SetDeadline(time.Time{})
	ok = true
	return &telegramWebSocketConn{Conn: c, reader: reader}, nil
}
