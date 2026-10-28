//go:build android || linux

package datapath

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
)

func TestFragmentedWhatsAppPreface(t *testing.T) {
	hello := append([]byte{'W', 'A', 6, 3, 0, 0, 36, 0x12, 34, 0x0a, 32}, make([]byte, 32)...)
	for split := 1; split < 11; split++ {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			go func() { defer b.Close(); b.Write(hello[:split]); b.Write(hello[split:]) }()
			first, err := readFirstPayload(a)
			if err != nil || !isWhatsAppNoiseStart(first) {
				t.Fatalf("header not recognized len=%d err=%v", len(first), err)
			}
			tail, err := io.ReadAll(a)
			if err != nil || !bytes.Equal(append(first, tail...), hello) {
				t.Fatal("stream changed")
			}
		})
	}
}

func TestFragmentedWhatsAppEdgePreface(t *testing.T) {
	hello := append([]byte{'E', 'D', 0, 1, 0, 0, 10}, make([]byte, 10)...)
	hello = append(hello, 'W', 'A', 6, 3, 0, 0, 36, 0x12, 34, 0x0a, 32)
	hello = append(hello, make([]byte, 32)...)
	for split := 1; split < 28; split++ {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			go func() { defer b.Close(); b.Write(hello[:split]); b.Write(hello[split:]) }()
			first, err := readFirstPayload(a)
			if err != nil || !isWhatsAppNoiseStart(first) || whatsappEdgeOffset(first) != 17 {
				t.Fatalf("edge header not recognized len=%d err=%v", len(first), err)
			}
			tail, err := io.ReadAll(a)
			if err != nil || !bytes.Equal(append(first, tail...), hello) {
				t.Fatal("stream changed")
			}
		})
	}
	if whatsappEdgeOffset([]byte{'E', 'D', 0, 1, 255, 255, 255}) != 0 {
		t.Fatal("unbounded edge accepted")
	}
}
