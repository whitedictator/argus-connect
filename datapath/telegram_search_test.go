package datapath

import (
	"testing"
	"time"
)

func TestTelegramSearchCooldownsAreIndependent(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	seen := map[int]bool{}
	for _, dc := range []int{1, 2, 3, 4, 5, -1, -2, -3, -4, -5} {
		slot := telegramSearchSlot(dc)
		if slot < 0 || slot >= 10 || seen[slot] {
			t.Fatalf("overlapping slot for %d", dc)
		}
		seen[slot] = true
	}
	p.nextSearch[telegramSearchSlot(1)] = time.Now().Add(16 * time.Minute)
	for _, dc := range []int{2, 4, -2, -4} {
		if time.Now().Before(p.nextSearch[telegramSearchSlot(dc)]) {
			t.Fatalf("DC1 suppressed %d", dc)
		}
	}
	p.configure("", true)
	if !p.nextSearch[0].IsZero() {
		t.Fatal("reconfigure retained stale cooldown")
	}
}
