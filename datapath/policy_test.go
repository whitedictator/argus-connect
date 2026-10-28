package datapath

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestServiceClassification(t *testing.T) {
	cases := map[string]serviceID{
		"api.telegram.org":    serviceTelegram,
		"e16.whatsapp.net":    serviceWhatsApp,
		"rr2.googlevideo.com": serviceYouTube,
		"chat.signal.org":     serviceSignal,
		"g-11.0.threema.ch":   serviceThreema,
		"unrelated.example":   serviceUnknown,
	}
	for host, expected := range cases {
		if actual := classifyDomain(host); actual != expected {
			t.Fatalf("classifyDomain(%q)=%q, want %q", host, actual, expected)
		}
	}
}

func TestThroughputPrefersProvenFastYouTubeRoute(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	policy.recordTraffic(serviceYouTube, "primary", strategyDirect, 400*1024, time.Second)
	policy.recordTraffic(serviceYouTube, "alternate-v6", strategyTLSRecord, 2*1024*1024, time.Second)
	if delay := policy.routeStartDelay(serviceYouTube, "alternate-v6"); delay != 0 {
		t.Fatalf("fast route delayed by %s", delay)
	}
	if delay := policy.routeStartDelay(serviceYouTube, "primary"); delay == 0 {
		t.Fatal("slow route was not deprioritized")
	}
}

func TestYouTubeStrategyUsesMeasuredThroughput(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	policy.enabled.Store(true)
	hello := clientHello("rr1.googlevideo.com")
	policy.recordTCP(serviceYouTube, strategyDirect, true)
	policy.recordTCP(serviceYouTube, strategyTLSRecord, true)
	policy.recordTraffic(serviceYouTube, "primary", strategyDirect, 400*1024, time.Second)
	policy.recordTraffic(serviceYouTube, "alternate-v6", strategyTLSRecord, 2*1024*1024, time.Second)
	if selected := policy.choose(serviceYouTube, hello); selected != strategyTLSRecord {
		t.Fatalf("selected %q, want faster %q", selected, strategyTLSRecord)
	}
}

func TestTelegramAddressClassification(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	if actual := policy.classifyIP(net.ParseIP("149.154.167.50")); actual != serviceTelegram {
		t.Fatalf("Telegram IP classified as %q", actual)
	}
}

func TestTLSServerNameAndRecordSplit(t *testing.T) {
	hello := clientHello("web.whatsapp.com")
	host, offset, ok := findTLSServerName(hello)
	if !ok || host != "web.whatsapp.com" {
		t.Fatalf("SNI parse got host=%q ok=%v", host, ok)
	}
	rewritten, boundary, ok := splitTLSRecord(hello, offset)
	if !ok || boundary <= 5 || len(rewritten) != len(hello)+5 {
		t.Fatalf("record split failed: ok=%v boundary=%d lengths=%d/%d", ok, boundary, len(hello), len(rewritten))
	}
}

func TestTLSMultiRecordPreservesHandshake(t *testing.T) {
	hello := clientHello("chat.signal.org")
	_, start, end, ok := findTLSServerNameBounds(hello)
	if !ok {
		t.Fatal("SNI bounds not found")
	}
	fragments, ok := tlsRecordFragments(hello, []int{start, start + (end-start)/2})
	if !ok || len(fragments) != 3 {
		t.Fatalf("multi-record split got ok=%v fragments=%d", ok, len(fragments))
	}
	joined := make([]byte, 0, len(hello)-5)
	for _, fragment := range fragments {
		length := int(binary.BigEndian.Uint16(fragment[3:5]))
		if length != len(fragment)-5 {
			t.Fatalf("record length=%d payload=%d", length, len(fragment)-5)
		}
		joined = append(joined, fragment[5:]...)
	}
	if string(joined) != string(hello[5:]) {
		t.Fatal("multi-record split changed ClientHello bytes")
	}
}

func TestFailedStrategiesRotateBeforeReuse(t *testing.T) {
	hello := clientHello("ds-apip.threema.ch")
	policy := newLightPolicy(&net.Dialer{})
	policy.enabled.Store(true)
	seen := make(map[strategyID]bool)
	for range 17 {
		strategy := policy.choose(serviceThreema, hello)
		if seen[strategy] {
			t.Fatalf("strategy %q repeated before candidate set was exhausted", strategy)
		}
		seen[strategy] = true
		policy.recordTCP(serviceThreema, strategy, false)
	}
}

func TestProvenStrategyDoesNotSacrificeEveryFifthLiveFlow(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	p.recordTCP(serviceYouTube, strategyComboFast, true)
	for i := 0; i < 25; i++ {
		if got := p.choose(serviceYouTube, clientHello("www.youtube.com")); got != strategyComboFast {
			t.Fatalf("live flow %d switched from proven strategy to %s", i, got)
		}
	}
	p.recordTCP(serviceYouTube, strategyComboFast, false)
	p.recordTCP(serviceYouTube, strategyComboFast, false)
	if got := p.choose(serviceYouTube, clientHello("www.youtube.com")); got == strategyComboFast {
		t.Fatal("failed method remained pinned")
	}
}

func TestWhatsAppTriesVerifiedCandidateEarly(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	hello := clientHello("mmg.whatsapp.net")
	if got := p.choose(serviceWhatsApp, hello); got != strategyDirect {
		t.Fatal("baseline skipped")
	}
	p.recordTCP(serviceWhatsApp, strategyDirect, false)
	if got := p.choose(serviceWhatsApp, hello); got != strategyOOB {
		t.Fatalf("second method=%s", got)
	}
}

func TestAlternateAddressStaysOnExactHost(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	policy.observeDNS("rr1.googlevideo.com", []net.IP{
		net.ParseIP("1.1.1.1"),
		net.ParseIP("1.0.0.1"),
	})

	got := policy.alternateAddress("rr1.googlevideo.com", net.ParseIP("1.1.1.1"))
	if got == nil || !got.Equal(net.ParseIP("1.0.0.1")) {
		t.Fatalf("alternate address=%v, want 1.0.0.1", got)
	}
	if got := policy.alternateAddress("other.googlevideo.com", net.ParseIP("1.1.1.1")); got != nil {
		t.Fatalf("address leaked across hostnames: %v", got)
	}
}

func TestYouTubeQUICFallsBackOnlyAfterRepeatedNoReply(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	policy.enabled.Store(true)
	if policy.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("fresh YouTube flow forced to TCP before QUIC was tested")
	}
	policy.recordUDP(serviceYouTube, false)
	if policy.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("one missing UDP reply forced TCP too early")
	}
	policy.recordUDP(serviceYouTube, false)
	if !policy.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("repeated missing UDP replies did not enable TCP fallback")
	}
	policy.recordUDP(serviceYouTube, true)
	if policy.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("successful QUIC reply did not clear TCP fallback")
	}
}

func TestMetaClassificationIsolatedFromWorkingServices(t *testing.T) {
	for _, host := range []string{"x.com", "api.twitter.com", "pbs.twimg.com", "video.twimg.com", "t.co"} {
		if classifyDomain(host) != serviceTwitter {
			t.Fatalf("unclassified %s", host)
		}
	}
	for _, host := range []string{"www.facebook.com", "i.instagram.com", "scontent.cdninstagram.com", "scontent-hel3-1.xx.fbcdn.net", "scontent-arn2-1.xx.fbcdn.net"} {
		if classifyDomain(host) != serviceMeta {
			t.Fatalf("unclassified %s", host)
		}
	}
	for host, want := range map[string]serviceID{"mmg.whatsapp.net": serviceWhatsApp, "scontent.xx.fbcdn.net": serviceWhatsApp, "googlevideo.com": serviceYouTube, "t.me": serviceTelegram, "evilinstagram.com": serviceUnknown, "facebook.com.example.org": serviceUnknown} {
		if classifyDomain(host) != want {
			t.Fatalf("changed classification %s", host)
		}
	}
}

func TestNAT64SynthesisRequiresAndroidPrefix(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	if got := policy.nat64Address(net.ParseIP("192.0.2.33")); got != nil {
		t.Fatalf("synthesized without NAT64 prefix: %v", got)
	}
	policy.setNAT64Prefix("64:ff9b::/96")
	got := policy.nat64Address(net.ParseIP("192.0.2.33"))
	if got == nil || got.String() != "64:ff9b::c000:221" {
		t.Fatalf("NAT64 address=%v, want 64:ff9b::c000:221", got)
	}
	policy.setNAT64Prefix("2001:db8::/64")
	if got := policy.nat64Address(net.ParseIP("192.0.2.33")); got != nil {
		t.Fatalf("accepted unsupported NAT64 prefix: %v", got)
	}
}

func TestFailureOnlyHistoryIsNotRestored(t *testing.T) {
	policy := newLightPolicy(&net.Dialer{})
	policy.loadState(`{"strategies":{"youtube":{"direct":{"attempts":100,"success":0,"failure":100,"recentFailure":100}}}}`)
	policy.enabled.Store(true)
	if selected := policy.choose(serviceYouTube, clientHello("www.youtube.com")); selected != strategyDirect {
		t.Fatalf("fresh session started with %q, want direct", selected)
	}
	policy.mu.RLock()
	_, restored := policy.scores[serviceYouTube][strategyDirect]
	policy.mu.RUnlock()
	if restored {
		t.Fatal("failure-only history was restored")
	}
}

func TestEmptyTLSRecordKeepsOriginalClientHello(t *testing.T) {
	hello := clientHello("www.youtube.com")
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	written := make(chan error, 1)
	go func() { written <- writeEmptyTLSRecord(client, hello) }()
	received := make([]byte, len(hello)+5)
	if _, err := io.ReadFull(server, received); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if string(received[:5]) != string([]byte{0x16, 3, 1, 0, 0}) {
		t.Fatalf("unexpected empty record prefix: %x", received[:5])
	}
	if string(received[5:]) != string(hello) {
		t.Fatal("empty-record strategy changed ClientHello")
	}
}

func TestSuccessfulStrategyIsReusedAndRestored(t *testing.T) {
	hello := clientHello("web.whatsapp.com")
	policy := newLightPolicy(&net.Dialer{})
	policy.enabled.Store(true)
	first := policy.choose(serviceWhatsApp, hello)
	if first != strategyDirect {
		t.Fatalf("first strategy=%q, want %q", first, strategyDirect)
	}
	policy.recordTCP(serviceWhatsApp, first, true)
	if next := policy.choose(serviceWhatsApp, hello); next != first {
		t.Fatalf("proven strategy not reused: got %q want %q", next, first)
	}

	restored := newLightPolicy(&net.Dialer{})
	restored.loadState(policy.statusJSON())
	restored.enabled.Store(true)
	if next := restored.choose(serviceWhatsApp, hello); next != first {
		t.Fatalf("restored strategy=%q, want %q", next, first)
	}
}

func clientHello(host string) []byte {
	sniName := append([]byte{0, byte(len(host) >> 8), byte(len(host))}, []byte(host)...)
	sniList := append([]byte{byte(len(sniName) >> 8), byte(len(sniName))}, sniName...)
	sniExtension := append([]byte{0, 0, byte(len(sniList) >> 8), byte(len(sniList))}, sniList...)
	extensions := append([]byte{byte(len(sniExtension) >> 8), byte(len(sniExtension))}, sniExtension...)
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0, 0, 2, 0x13, 0x01, 1, 0)
	body = append(body, extensions...)
	handshake := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	handshake = append(handshake, body...)
	record := []byte{0x16, 3, 1, 0, 0}
	binary.BigEndian.PutUint16(record[3:], uint16(len(handshake)))
	return append(record, handshake...)
}

func TestTelegramCallCandidatesSurviveOtherUDPFailures(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	for attempt := 0; attempt < 5; attempt++ {
		for _, port := range []uint16{443, 3478, 599, 14000} {
			if p.shouldForceTCP(serviceTelegram, port) {
				t.Fatalf("Telegram voice UDP port %d suppressed after %d failures", port, attempt)
			}
		}
		p.recordUDP(serviceTelegram, false)
	}
	if p.udp[serviceTelegram].Fallback {
		t.Fatal("Telegram must not advertise a TCP fallback that does not exist")
	}
	// Keep the independent YouTube QUIC fallback intact.
	p.recordUDP(serviceYouTube, false)
	p.recordUDP(serviceYouTube, false)
	if !p.shouldForceTCP(serviceYouTube, 443) {
		t.Fatal("YouTube fallback regressed")
	}
}

func TestVoiceMappingTrialIsNeverPersisted(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	if p.voiceMappingTrial.Load() {
		t.Fatal("experimental mapping enabled by default")
	}
	p.voiceMappingTrial.Store(true)
	p.recordConnect(serviceTelegram, true, "existing-message-route")
	saved := p.strategyMemoryJSON()
	restored := newLightPolicy(&net.Dialer{})
	defer restored.mtproxies.cancel()
	restored.loadState(saved)
	if restored.voiceMappingTrial.Load() {
		t.Fatal("ephemeral voice experiment persisted across launch")
	}
	if p.connect[serviceTelegram].Route != "existing-message-route" {
		t.Fatal("voice switch changed Telegram TCP route")
	}
}
