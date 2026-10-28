package datapath

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestDiscoveredEvidenceStaysOnRequestedDC(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	c := mtProxyConfig{id: "candidate"}
	p.health["candidate/2"] = mtProxyHealth{address: "original"}
	for _, dc := range []int{4, -2, -4} {
		if !p.recordDiscoveredLocked(c, dc, "8.8.8.8:443", time.Millisecond) {
			t.Fatal("candidate rejected")
		}
		if p.health[fmt.Sprintf("candidate/%d", dc)].verified.IsZero() {
			t.Fatalf("missing target DC %d", dc)
		}
	}
	if p.health["candidate/2"].address != "original" || !p.health["candidate/2"].verified.IsZero() {
		t.Fatal("unverified DC2 was overwritten/promoted")
	}
	if len(p.configs) != 1 {
		t.Fatal("duplicate candidate")
	}
}

func TestFullDiscoveryPoolDoesNotClaimUnusableRoute(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	for i := 0; i < 12; i++ {
		p.configs = append(p.configs, mtProxyConfig{id: fmt.Sprint(i)})
	}
	if p.recordDiscoveredLocked(mtProxyConfig{id: "extra"}, 4, "8.8.8.8:443", time.Millisecond) {
		t.Fatal("claimed unusable extra route")
	}
	if len(p.health) != 0 {
		t.Fatal("retained unusable route evidence")
	}
}

func TestMTProxyDiscoveryFailureDoesNotEraseIndependentDCEvidence(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	now := time.Now()
	p.health["example/2"] = mtProxyHealth{verified: now}
	p.health["example/-2"] = mtProxyHealth{verified: now}
	p.health["example/1"] = mtProxyHealth{verified: now.Add(-3 * time.Minute)}
	p.health["example/4"] = mtProxyHealth{verified: now, nextTry: now.Add(time.Minute)}
	p.lastStage = "search-exhausted"
	var status struct {
		Verified []int `json:"verifiedMainDcs"`
	}
	if err := json.Unmarshal(p.statusJSON(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Verified) != 1 || status.Verified[0] != 2 {
		t.Fatalf("wrong fresh main DC evidence: %v", status.Verified)
	}
	p.report("example", 2, false)
	if err := json.Unmarshal(p.statusJSON(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Verified) != 0 {
		t.Fatal("actual DC failure kept stale evidence")
	}
}

func TestMTProxyExactDCMapping(t *testing.T) {
	for address, want := range map[string]int{
		"149.154.167.41": 2, "149.154.167.92": 4,
		"149.154.175.57": 1, "91.108.56.195": 5,
		"149.154.167.222": -2, "149.154.165.111": -4,
		// Fresh RSA/DH-verified public help.getConfig, 2026-09-14 Moscow.
		"149.154.167.43": -4, "149.154.175.55": 1, "91.108.56.177": 5,
		"2001:067c:04e8:f002:0000:0000:0000:000b": -2,
		"149.154.167.42": 0, "8.8.8.8": 0,
	} {
		if got := telegramProxyDC(net.ParseIP(address)); got != want {
			t.Fatalf("%s: DC%d want DC%d", address, got, want)
		}
	}
}

func TestMTProxyBatchRotation(t *testing.T) {
	configs := []mtProxyConfig{{id: "a"}, {id: "b"}, {id: "c"}, {id: "d"}, {id: "e"}}
	cursor := 0
	for _, want := range []string{"abc", "dea", "bcd", "eab"} {
		var batch []mtProxyConfig
		batch, cursor = mtProxyBatch(configs, cursor, 3)
		got := ""
		for _, c := range batch {
			got += c.id
		}
		if got != want {
			t.Fatalf("batch %s want %s", got, want)
		}
	}
	if batch, next := mtProxyBatch(nil, 20, 18); len(batch) != 0 || next != 0 {
		t.Fatal("empty list")
	}
	if batch, _ := mtProxyBatch(configs, 0, 18); len(batch) != 5 {
		t.Fatal("duplicate probes in small list")
	}
}

func TestMTProxyFailureInvalidatesCurrentRoute(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	p.active = "example"
	p.health["example/2"] = mtProxyHealth{verified: time.Now(), address: "8.8.8.8:443"}
	p.report("example", 2, false)
	if p.active != "" || p.lastStage != "route-failed" || !p.health["example/2"].verified.IsZero() {
		t.Fatal("failed route remained active or verified")
	}
	if !p.health["example/2"].nextTry.After(time.Now()) {
		t.Fatal("missing retry cooldown")
	}
}

func TestMTProxyReuseAddressForReverificationUntilFailure(t *testing.T) {
	now := time.Now()
	h := mtProxyHealth{verified: now.Add(-time.Minute), address: "8.8.8.8:443"}
	if mtProxyReusableAddress(h, now) != h.address {
		t.Fatal("rotated away from verified endpoint")
	}
	if mtProxyReusableAddress(h, now.Add(2*time.Minute)) != h.address {
		t.Fatal("known endpoint lost instead of being reverified")
	}
	h.nextTry = now.Add(time.Second)
	if mtProxyReusableAddress(h, now) != "" {
		t.Fatal("failed endpoint reused during cooldown")
	}
	h.nextTry = time.Time{}
	h.verified = time.Time{}
	if mtProxyReusableAddress(h, now) != "" {
		t.Fatal("invalidated endpoint reused")
	}
}

func TestMTProxyFailureWaitCanBeCancelled(t *testing.T) {
	p := newMTProxyPool()
	p.cancel()
	start := time.Now()
	p.waitAfterDialFailure(start)
	if time.Since(start) > time.Second {
		t.Fatal("shutdown stalled by retry backoff")
	}
}

func TestMTProxyFailureWaitPreventsBusyReconnect(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	start := time.Now().Add(-1900 * time.Millisecond)
	p.waitAfterDialFailure(start)
	if time.Since(start) < 2*time.Second {
		t.Fatal("immediate failure permits reconnect storm")
	}
}
