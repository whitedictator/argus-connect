package datapath

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
)

type fallbackDNSRoundTripper func(*http.Request) (*http.Response, error)

func (f fallbackDNSRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIPv6TLSFallbackUsesExactDNSName(t *testing.T) {
	calls := 0
	r := &dohResolver{endpoints: []dohEndpoint{{name: "dns.example", client: &http.Client{Transport: fallbackDNSRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		q, _ := io.ReadAll(req.Body)
		name, _, err := parseDNSAnswer(q)
		if err != nil || name != "www.google.com" || q[len(q)-3] != 1 {
			t.Fatalf("wrong DNS question: %s %v", name, err)
		}
		q[2], q[3], q[7] = 0x81, 0x80, 1
		q = append(q, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 142, 251, 151, 119)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(q)), Header: make(http.Header)}, nil
	})}}}}
	p := &lightPolicy{doh: r}
	got := p.withIPv4Fallback("www.google.com", nil)
	if len(got) != 1 || got[0].String() != "142.251.151.119" {
		t.Fatalf("lost exact IPv4: %v", got)
	}
	p.withIPv4Fallback("www.google.com", got)
	p.withIPv4Fallback("", nil)
	if calls != 1 {
		t.Fatalf("unnecessary lookup: %d", calls)
	}
	if classifyDomain("www.google.com") != serviceUnknown || isGeminiDNSName("www.google.com") {
		t.Fatal("search inherited Gemini policy")
	}
	if !got[0].Equal(net.ParseIP("142.251.151.119")) {
		t.Fatal("address changed")
	}
}
