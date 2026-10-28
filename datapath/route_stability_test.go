package datapath

import (
	"net"
	"testing"
	"time"
)

func TestSmallYouTubeReplyDoesNotEvictOrPromote(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.recordTCPOutcome(serviceYouTube, strategyTLSRecord, 65536)
	for i := 0; i < 10; i++ {
		p.recordTCPOutcome(serviceYouTube, strategyTLSRecord, 792)
		p.recordTCPOutcome(serviceYouTube, strategyDirect, 792)
	}
	r := p.scores[serviceYouTube][strategyTLSRecord]
	if r.Success != 1 || r.Failure != 0 || r.RecentFailure != 0 {
		t.Fatalf("small reply penalized working method: %+v", r)
	}
	if p.scores[serviceYouTube][strategyDirect] != nil {
		t.Fatal("small reply promoted unknown method")
	}
	p.recordTCPOutcome(serviceYouTube, strategyTLSRecord, 0)
	if r.Failure != 1 {
		t.Fatal("zero-byte failure ignored")
	}
}

func TestActiveVerifiedRouteRefreshesWithoutCrossHostChanges(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.verifiedServices = map[string]verifiedServiceRoute{
		"www.youtube.com": {Address: "142.250.1.1:443", Expires: time.Now().Add(time.Second)},
		"chat.signal.org": {Address: "76.223.92.165:443", Expires: time.Now().Add(-time.Second)},
	}
	p.reportVerifiedRoute("www.youtube.com", "142.250.1.1:443", true)
	if time.Until(p.verifiedServices["www.youtube.com"].Expires) < time.Minute {
		t.Fatal("active route not refreshed")
	}
	if _, ok := p.verifiedRoute("chat.signal.org", 443); ok {
		t.Fatal("unrelated idle route revived")
	}
	p.reportVerifiedRoute("www.youtube.com", "142.250.1.1:443", false)
	p.reportVerifiedRoute("www.youtube.com", "142.250.1.1:443", false)
	if _, ok := p.verifiedRoute("www.youtube.com", 443); ok {
		t.Fatal("failed route pinned forever")
	}
}

func TestKnownReplyDoesNotDisableAllYouTubeQUIC(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	p.recordUDP(serviceYouTube, true)
	p.recordUDP(serviceYouTube, false)
	if p.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("single failure forced fallback")
	}
	p.recordUDP(serviceYouTube, false)
	if p.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("unrelated no-reply flows disabled a responding YouTube UDP path")
	}
	if p.shouldForceTCP(serviceYouTube, 3478) || p.shouldForceTCP(serviceMeta, 443) {
		t.Fatal("fallback escaped YouTube QUIC")
	}
	p.recordUDP(serviceYouTube, true)
	if p.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("reply did not clear fallback")
	}
}
