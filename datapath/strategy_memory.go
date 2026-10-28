package datapath

import (
	"encoding/json"
	"strings"
)

func channelPolicyKey(service serviceID, host string) serviceID {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	under := func(s string) bool { return host == s || strings.HasSuffix(host, "."+s) }
	switch service {
	case serviceTwitter:
		if host == "pbs.twimg.com" {
			return "twitter/photos"
		}
		if host == "video.twimg.com" {
			return "twitter/video"
		}
		if under("twimg.com") {
			return "twitter/assets"
		}
		if host != "" {
			return "twitter/feed"
		}
	case serviceMeta:
		if under("cdninstagram.com") || under("fbcdn.net") {
			return "meta/media"
		}
		if host != "" {
			return "meta/web"
		}
	case serviceYouTube:
		if under("googlevideo.com") {
			return "youtube/video"
		}
		if under("ytimg.com") || under("ggpht.com") {
			return "youtube/thumbnails"
		}
		if host != "" {
			return "youtube/feed"
		}
	case serviceWhatsApp:
		if under("fbcdn.net") || strings.HasPrefix(host, "mmg.") || strings.HasPrefix(host, "media.") || host == "static.whatsapp.net" {
			return "whatsapp/media"
		}
		if host != "" {
			return "whatsapp/messages"
		}
	case serviceSignal:
		if host == "chat.signal.org" || host == "grpc.chat.signal.org" {
			return "signal/messages"
		}
		if host == "cdn.signal.org" || host == "cdn2.signal.org" || host == "cdn3.signal.org" {
			return "signal/media"
		}
		if under("voip.signal.org") {
			return "signal/calls"
		}
	}
	return service
}

func basePolicyService(service serviceID) serviceID {
	return serviceID(strings.SplitN(string(service), "/", 2)[0])
}

func (p *lightPolicy) chooseChannel(service, channel serviceID, payload []byte) strategyID {
	p.mu.RLock()
	hasOwnEvidence := len(p.scores[channel]) > 0
	hasOwnSuccess := false
	for _, outcome := range p.scores[channel] {
		hasOwnSuccess = hasOwnSuccess || (outcome.Success > 0 && outcome.RecentFailure < 2)
	}
	p.mu.RUnlock()
	if channel != service && !hasOwnEvidence {
		// Bootstrap from the existing service policy without copying its
		// success to the new channel. Later outcomes are learned separately.
		return p.choose(service, payload)
	}
	if channel != service && !hasOwnSuccess {
		// A channel may have failed before another channel discovered a working
		// tactic. Reuse that substantial-traffic evidence as a candidate, without
		// copying success or hiding two failures on this particular channel.
		candidate := p.choose(service, payload)
		p.mu.Lock()
		outcome := p.scores[service][candidate]
		local := p.scores[channel][candidate]
		measured := p.warmStrategies[service][candidate]
		if traffic := p.traffic[service]; traffic != nil {
			measured = measured || traffic.StrategyKbps[candidate] > 0
		}
		eligible := measured && outcome != nil && outcome.Success > 0 && outcome.RecentFailure < 2 && (local == nil || local.RecentFailure < 2)
		if eligible {
			p.current[channel] = candidate
		}
		p.mu.Unlock()
		if eligible {
			return candidate
		}
	}
	return p.choose(channel, payload)
}

// Persist only aggregate tactics with substantial received traffic. TLS alerts,
// SYN/ACKs and a small handshake cannot become a next-session recommendation.
// No destinations, domains, proxy secrets or account metadata are serialized.
func (p *lightPolicy) strategyMemoryJSON() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	saved := make(map[serviceID]map[strategyID]*strategyResult)
	for service, strategies := range p.warmStrategies {
		for strategy := range strategies {
			outcome := p.scores[service][strategy]
			if outcome == nil || outcome.RecentFailure >= 2 {
				continue
			}
			if saved[service] == nil {
				saved[service] = make(map[strategyID]*strategyResult)
			}
			saved[service][strategy] = &strategyResult{Attempts: 1, Success: 1}
		}
	}
	for service, traffic := range p.traffic {
		for strategy, speed := range traffic.StrategyKbps {
			outcome := p.scores[service][strategy]
			if speed <= 0 || (outcome != nil && outcome.RecentFailure >= 2) {
				continue
			}
			if saved[service] == nil {
				saved[service] = make(map[strategyID]*strategyResult)
			}
			saved[service][strategy] = &strategyResult{Attempts: 1, Success: 1}
		}
	}
	encoded, _ := json.Marshal(struct {
		Scores map[serviceID]map[strategyID]*strategyResult `json:"strategies"`
	}{saved})
	return string(encoded)
}
