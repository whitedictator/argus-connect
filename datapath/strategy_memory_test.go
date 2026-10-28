package datapath

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestMemoryRejectsHandshakeAndSeparatesChannels(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	p.recordTCP("signal/messages", strategyDisorder, true)
	if strings.Contains(p.strategyMemoryJSON(), "signal/messages") {
		t.Fatal("handshake promoted to persistent strategy")
	}
	p.recordTraffic("youtube/video", "primary", strategyComboFast, 262144, time.Second)
	p.recordTraffic("youtube/feed", "primary", strategyDisorder, 65536, time.Second)
	snapshot := p.strategyMemoryJSON()
	q := newLightPolicy(&net.Dialer{})
	defer q.mtproxies.cancel()
	q.enabled.Store(true)
	q.loadState(snapshot)
	if got := q.chooseChannel(serviceYouTube, "youtube/video", clientHello("a.googlevideo.com")); got != strategyComboFast {
		t.Fatalf("video lost priority: %s", got)
	}
	if got := q.chooseChannel(serviceYouTube, "youtube/feed", clientHello("youtubei.googleapis.com")); got != strategyDisorder {
		t.Fatalf("feed inherited video: %s", got)
	}
	if q.strategyMemoryJSON() != snapshot {
		t.Fatal("unused remembered tactics vanished before recheck")
	}
	q.recordTCP("youtube/video", strategyComboFast, false)
	q.recordTCP("youtube/video", strategyComboFast, false)
	if got := q.chooseChannel(serviceYouTube, "youtube/video", clientHello("a.googlevideo.com")); got == strategyComboFast {
		t.Fatal("failed remembered tactic remained locked")
	}
	if strings.Contains(q.strategyMemoryJSON(), "youtube/video") {
		t.Fatal("failed tactic re-persisted")
	}
	if got := q.chooseChannel(serviceYouTube, "youtube/feed", clientHello("youtubei.googleapis.com")); got != strategyDisorder {
		t.Fatal("video failure damaged feed")
	}
}

func TestPolicyChannelClassification(t *testing.T) {
	for _, tc := range []struct {
		service serviceID
		host    string
		want    serviceID
	}{
		{serviceYouTube, "rr1.googlevideo.com", "youtube/video"},
		{serviceYouTube, "i.ytimg.com", "youtube/thumbnails"},
		{serviceYouTube, "youtubei.googleapis.com", "youtube/feed"},
		{serviceWhatsApp, "mmg.whatsapp.net", "whatsapp/media"},
		{serviceWhatsApp, "", "whatsapp"},
		{serviceSignal, "grpc.chat.signal.org", "signal/messages"},
		{serviceSignal, "cdn3.signal.org", "signal/media"},
		{serviceSignal, "sfu.voip.signal.org", "signal/calls"},
		{serviceMeta, "www.instagram.com", "meta/web"},
		{serviceTwitter, "pbs.twimg.com", "twitter/photos"},
		{serviceTwitter, "video.twimg.com", "twitter/video"},
		{serviceTwitter, "api.x.com", "twitter/feed"},
	} {
		if got := channelPolicyKey(tc.service, tc.host); got != tc.want {
			t.Errorf("%s: %s != %s", tc.host, got, tc.want)
		}
	}
}

func TestFailedChannelReusesNewMeasuredParentTacticWithoutPinning(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	hello := clientHello("scontent.example.cdninstagram.com")
	channel := serviceID("meta/media")
	p.recordTCP(channel, strategyDirect, false)
	p.recordTCP(serviceMeta, strategyComboFast, true)
	// A handshake alone must not override this channel's search.
	if got := p.chooseChannel(serviceMeta, channel, hello); got == strategyComboFast {
		t.Fatal("unmeasured parent evidence promoted")
	}
	p.recordTraffic(serviceMeta, "primary", strategyComboFast, 65536, time.Second)
	if got := p.chooseChannel(serviceMeta, channel, hello); got != strategyComboFast {
		t.Fatalf("failed channel ignored newly working parent: %s", got)
	}
	if p.scores[channel][strategyComboFast] != nil {
		t.Fatal("parent success copied into untested channel")
	}
	p.recordTCP(channel, strategyComboFast, false)
	p.recordTCP(channel, strategyComboFast, false)
	if got := p.chooseChannel(serviceMeta, channel, hello); got == strategyComboFast {
		t.Fatal("parent overrode repeated channel failures")
	}
}
