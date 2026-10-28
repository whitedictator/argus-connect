package datapath

import (
	"net"
	"testing"
)

func TestExperimentalMediaMappingsAreExact(t *testing.T) {
	for ip, want := range map[string]int{
		"149.154.162.123": -2, "149.154.167.255": -4,
		"149.154.162.122": 0, "149.154.167.254": 0,
		"149.154.167.50": 2, "149.154.167.92": 4,
	} {
		if got := telegramProxyDC(net.ParseIP(ip)); got != want {
			t.Fatalf("%s: got %d want %d", ip, got, want)
		}
	}
}
