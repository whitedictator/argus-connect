//go:build linux

package datapath

import (
	"bytes"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestFlowDestinationIPv4(t *testing.T) {
	id := stack.TransportEndpointID{
		LocalAddress: tcpip.AddrFrom4([4]byte{203, 0, 113, 7}),
		LocalPort:    443,
	}
	if got := flowDestination(id); got != "203.0.113.7:443" {
		t.Fatalf("destination = %q", got)
	}
}

func TestTrafficMeterPublishesBeforeFlowCloses(t *testing.T) {
	var destination bytes.Buffer
	var sampled int64
	meter := &trafficMeter{
		destination: &destination,
		onSample: func(bytes int64, _ time.Duration) {
			sampled += bytes
		},
		started: time.Now().Add(-2 * time.Second),
	}
	payload := make([]byte, 40*1024)
	if count, err := meter.Write(payload); err != nil || count != len(payload) {
		t.Fatalf("write count=%d err=%v", count, err)
	}
	if sampled != int64(len(payload)) {
		t.Fatalf("sampled=%d, want %d", sampled, len(payload))
	}
}

func TestNetworkStartsClosed(t *testing.T) {
	selector := &networkSelector{}
	if err := selector.control("tcp", "example", nil); err == nil {
		t.Fatal("host selector unexpectedly allowed an unbound socket")
	}
}

func TestYouTubeRequiresMeaningfulBodyBeforeStrategyPromotion(t *testing.T) {
	if meaningfulTCPResponse(serviceYouTube, 4*1024) {
		t.Fatal("short YouTube TLS response was treated as success")
	}
	if !meaningfulTCPResponse(serviceYouTube, 32*1024) {
		t.Fatal("meaningful YouTube body was not treated as success")
	}
	if !meaningfulTCPResponse(serviceTelegram, 1) {
		t.Fatal("non-YouTube service response was rejected")
	}
}
