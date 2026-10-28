//go:build android || linux

package datapath

import (
	"net"
	"testing"
)

type noReadDiagnosticConn struct{ net.Conn }

func (noReadDiagnosticConn) Read([]byte) (int, error) {
	panic("unmapped diagnostic consumed fallback bytes")
}

func TestUnmappedTelegramDoesNotConsumeFallbackStream(t *testing.T) {
	dialer := &net.Dialer{}
	policy := newLightPolicy(dialer)
	defer policy.mtproxies.cancel()
	// An incomplete obfuscation header used to cause a read in classification.
	if handleTelegramMTProxy(noReadDiagnosticConn{}, dialer, net.ParseIP("149.154.162.122"), []byte{1}, policy) {
		t.Fatal("unmapped endpoint must remain available to fallback")
	}
}
