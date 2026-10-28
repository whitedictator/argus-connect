package datapath

import (
	"net"
	"net/netip"
	"testing"
)

func TestSingleAlternativeForCachedIPv6(t *testing.T) {
	p := &lightPolicy{
		domainHints:   map[string][]netip.Addr{"www.instagram.com": {netip.MustParseAddr("157.240.205.63")}},
		routeRotation: make(map[string]int),
	}
	got := p.alternateAddresses("www.instagram.com", net.ParseIP("2a03:2880:f213:e4:face:b00c:0:4420"), 2)
	if len(got) != 1 || got[0].String() != "157.240.205.63" {
		t.Fatalf("single v4 alternative lost: %v", got)
	}
	if got := p.alternateAddresses("www.instagram.com", net.ParseIP("157.240.205.63"), 2); len(got) != 0 {
		t.Fatalf("original repeated: %v", got)
	}
	if got := p.alternateAddresses("mmg.whatsapp.net", net.ParseIP("157.240.205.63"), 2); len(got) != 0 {
		t.Fatalf("cross-domain fallback: %v", got)
	}
}
