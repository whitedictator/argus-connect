package datapath

import (
	"context"
	"net"
	"testing"
)

func TestExplicitRelayValidation(t *testing.T) {
	for _, input := range []string{"wss://kws2.pclead.co.uk/apiws", "wss://kws4.cakeisalie.co.uk/apiws"} {
		c, ok := parseMTProxy(input)
		if !ok || c.wsDC == 0 || c.port != "443" {
			t.Fatalf("rejected valid relay %q", input)
		}
		if conn, err := openConfiguredMTProxy(context.Background(), &net.Dialer{}, "127.0.0.1:1", c, 1); err == nil || conn != nil {
			t.Fatal("must reject DC mismatch before dialing")
		}
	}
	for _, input := range []string{"wss://kws0.pclead.co.uk/apiws", "wss://kws2.pclead.co.uk.evil.test/apiws", "wss://user@kws2.pclead.co.uk/apiws", "wss://kws2.pclead.co.uk:80/apiws", "wss://kws2.pclead.co.uk/apiws?x=1", "wss://127.0.0.1/apiws"} {
		if _, ok := parseMTProxy(input); ok {
			t.Fatalf("accepted invalid relay %q", input)
		}
	}
}
