//go:build android || linux

package datapath

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestFragmentedTLSRecordHeader(t *testing.T) {
	for _, split := range []int{1, 2, 3, 4} {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			hello := clientHello("chat.signal.org")
			go func() { b.Write(hello[:split]); b.Write(hello[split:]) }()
			got, err := readFirstPayload(a)
			if err != nil || !bytes.Equal(got, hello) {
				t.Fatalf("fragmented record: len=%d err=%v", len(got), err)
			}
			if host, _, ok := findTLSServerName(got); !ok || host != "chat.signal.org" {
				t.Fatal("lost SNI")
			}
		})
	}
}

// End-to-end TLS + HTTP through the actual forwarding function, without DNS
// hints. The server must receive the unmodified application stream and return
// a 256 KiB body. A synthetic local CA is trusted ONLY by this test client.
func TestForwardTLSWithoutDNSHints(t *testing.T) {
	body := bytes.Repeat([]byte("argus-body"), 32768)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	// Use an exact-host verified hint to the local test listener; if the
	// forwarder skips SNI because DNS was cached, it will dial port 443 instead.
	p.verifiedServices = map[string]verifiedServiceRoute{"example.com": {Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Strategy: strategyTLSRecord, Expires: time.Now().Add(time.Minute)}}
	app, forward := net.Pipe()
	defer app.Close()
	go handleTCP(forward, &net.Dialer{}, net.ParseIP("127.0.0.1"), 443, p)
	config := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	config.ServerName = "example.com"
	c := tls.Client(app, config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Connection", "close")
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil || !bytes.Equal(body, got) {
		t.Fatalf("body mismatch len=%d err=%v", len(got), err)
	}
}
