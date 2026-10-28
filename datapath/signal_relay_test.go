package datapath

import "testing"

func TestSignalRelayScope(t *testing.T) {
	for _, host := range []string{"chat.signal.org", "grpc.chat.signal.org"} {
		if !signalRelayHost(host, 443) || signalRelayHost(host, 5222) {
			t.Fatal(host)
		}
	}
	for _, host := range []string{"", "api.whatsapp.net", "chat.signal.org.example.com", "example.com", "sfu.voip.signal.org"} {
		if signalRelayHost(host, 443) {
			t.Fatal(host)
		}
	}
	p := newLightPolicy(nil)
	defer p.mtproxies.cancel()
	if p.signalRelay.Load() {
		t.Fatal("relay must be opt-in")
	}
}
