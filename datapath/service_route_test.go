package datapath

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifiedRouteExactScopeExpiryAndFailures(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.verifiedServices = map[string]verifiedServiceRoute{"chat.signal.org": {Address: "76.223.92.165:443", Strategy: strategyTLSRecord, Expires: time.Now().Add(time.Minute)}}
	if _, ok := p.verifiedRoute("chat.signal.org", 443); !ok {
		t.Fatal("exact route missing")
	}
	if _, ok := p.verifiedRoute("cdn.signal.org", 443); ok {
		t.Fatal("cross-host routing")
	}
	if _, ok := p.verifiedRoute("chat.signal.org", 80); ok {
		t.Fatal("cross-port routing")
	}
	p.reportVerifiedRoute("chat.signal.org", "unrelated:443", false)
	if _, ok := p.verifiedRoute("chat.signal.org", 443); !ok {
		t.Fatal("stale flow changed route")
	}
	p.reportVerifiedRoute("chat.signal.org", "76.223.92.165:443", false)
	p.reportVerifiedRoute("chat.signal.org", "76.223.92.165:443", false)
	if _, ok := p.verifiedRoute("chat.signal.org", 443); ok {
		t.Fatal("failed route retained")
	}
	r := p.verifiedServices["chat.signal.org"]
	r.Failures, r.Expires = 0, time.Now().Add(-time.Second)
	p.verifiedServices["chat.signal.org"] = r
	if _, ok := p.verifiedRoute("chat.signal.org", 443); ok {
		t.Fatal("expired route retained")
	}
}

func TestServiceRouteRejectsUnapprovedTarget(t *testing.T) {
	for _, host := range []string{"grpc.chat.signal.org", "api.whatsapp.net", "youtube.googleapis.com"} {
		if !serviceProbeHost(host) {
			t.Fatalf("missing host %s", host)
		}
	}
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	if serviceProbeHost("localhost") || serviceProbeHost("chat.signal.org.attacker.invalid") {
		t.Fatal("host allowlist bypass")
	}
	if got := p.probeServiceRoute("localhost", ""); got != `{"host":"localhost","stage":"host-not-allowed","verified":false}` {
		t.Fatal(got)
	}
}

func TestWorkingRouteSurvivesShortProbeGapButFailsClosed(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.verifiedServices = map[string]verifiedServiceRoute{"www.instagram.com": {Address: "57.144.244.34:443", Strategy: strategyDisorder, Expires: time.Now().Add(time.Minute)}}
	p.reportVerifiedRoute("www.instagram.com", "57.144.244.34:443", true)
	r, _ := p.verifiedRoute("www.instagram.com", 443)
	if time.Until(r.Expires) < 14*time.Minute {
		t.Fatal("healthy route expires before transient maintenance can recover")
	}
	p.reportVerifiedRoute("www.instagram.com", "57.144.244.34:443", false)
	if _, ok := p.verifiedRoute("www.instagram.com", 443); !ok {
		t.Fatal("single failed flow discarded route")
	}
	p.reportVerifiedRoute("www.instagram.com", "57.144.244.34:443", false)
	if _, ok := p.verifiedRoute("www.instagram.com", 443); ok {
		t.Fatal("two real failures kept failed route")
	}
}

func TestOldTacticCannotInvalidateReplacementOnSameAddress(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.verifiedServices = map[string]verifiedServiceRoute{"www.youtube.com": {Address: "142.251.151.4:443", Strategy: strategyComboFast, Expires: time.Now().Add(time.Minute)}}
	for i := 0; i < 3; i++ {
		p.reportVerifiedRoute("www.youtube.com", "142.251.151.4:443", false, strategyTLSRecord)
	}
	if _, ok := p.verifiedRoute("www.youtube.com", 443); !ok {
		t.Fatal("old in-flight tactic invalidated replacement")
	}
	for i := 0; i < 2; i++ {
		p.reportVerifiedRoute("www.youtube.com", "142.251.151.4:443", false, strategyComboFast)
	}
	if _, ok := p.verifiedRoute("www.youtube.com", 443); ok {
		t.Fatal("actual replacement failures ignored")
	}
}

func TestYouTubeProbeRequiresChromiumTLSVerifiedCertificateAndBody(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"www.youtube.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.TLS.ServerName != "www.youtube.com" || r.Header.Get("Cache-Control") == "" {
			t.Error("probe lost HTTP/2, exact SNI, or no-cache")
		}
		w.Write([]byte("User-agent: *"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	for _, trusted := range []bool{true, false} {
		config := &tls.Config{ServerName: "www.youtube.com", RootCAs: roots}
		if !trusted {
			config.RootCAs = x509.NewCertPool()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stage, code, n, err := probeServiceHTTP(ctx, &net.Dialer{}, strings.TrimPrefix(server.URL, "https://"), "www.youtube.com", strategyComboFast, config)
		cancel()
		if trusted && (err != nil || code != 200 || n == 0 || stage != "HTTP2-Chromium") {
			t.Fatalf("stage=%s code=%d bytes=%d err=%v", stage, code, n, err)
		}
		if !trusted && err == nil {
			t.Fatal("untrusted certificate accepted")
		}
	}
}
