package datapath

import (
	"encoding/binary"
	"testing"
)

func TestYouTubeHedgeRejectsEarlyDataAndExtraRecords(t *testing.T) {
	hello := clientHello("rr1.googlevideo.com")
	if !safeYouTubeHello(hello, "rr1.googlevideo.com") {
		t.Fatal("complete test hello rejected")
	}
	for i := 0; i < len(hello); i++ {
		if safeYouTubeHello(hello[:i], "rr1.googlevideo.com") {
			t.Fatal("truncated hello accepted")
		}
	}
	if safeYouTubeHello(append(append([]byte{}, hello...), 23, 3, 3, 0, 1, 9), "rr1.googlevideo.com") {
		t.Fatal("coalesced application data replayable")
	}
	if safeYouTubeHello(hello, "chat.signal.org") {
		t.Fatal("other service affected")
	}
	// Append early_data and update enclosing lengths without copying app bytes.
	early := append(append([]byte{}, hello...), 0, 42, 0, 0)
	binary.BigEndian.PutUint16(early[3:5], uint16(len(early)-5))
	n := len(early) - 9
	early[6] = byte(n >> 16)
	early[7] = byte(n >> 8)
	early[8] = byte(n)
	p := 43
	p += 1 + int(early[p])
	p += 2 + int(binary.BigEndian.Uint16(early[p:]))
	p += 1 + int(early[p])
	binary.BigEndian.PutUint16(early[p:], binary.BigEndian.Uint16(early[p:])+4)
	if safeYouTubeHello(early, "rr1.googlevideo.com") {
		t.Fatal("TLS early_data accepted for replay")
	}
}
