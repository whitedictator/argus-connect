package datapath

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestYouTubeHandshakeRaceDoesNotReplayApplication(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	hello := clientHello("rr1.googlevideo.com")
	payload := []byte("unique-application-request")
	var accepted, application atomic.Int32
	done := make(chan struct{}, 4)
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			index := accepted.Add(1)
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				var handshake []byte
				for len(handshake) < len(hello)-5 {
					header := make([]byte, 5)
					if _, e := io.ReadFull(c, header); e != nil {
						return
					}
					if header[0] != 22 {
						return
					}
					record := make([]byte, int(header[3])<<8|int(header[4]))
					if _, e := io.ReadFull(c, record); e != nil {
						return
					}
					handshake = append(handshake, record...)
				}
				if !bytes.Equal(handshake, hello[5:]) {
					return
				}
				if index > 1 {
					reply := make([]byte, 47)
					reply[0] = 22
					reply[1] = 3
					reply[2] = 3
					reply[4] = 42
					reply[5] = 2
					reply[8] = 38
					c.Write(reply)
				}
				b := make([]byte, len(payload))
				if _, e := io.ReadFull(c, b); e == nil && bytes.Equal(b, payload) {
					application.Add(1)
				}
				done <- struct{}{}
			}()
		}
	}()
	initial, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err := raceYouTubeHello(context.Background(), &net.Dialer{Timeout: time.Second}, initial, hello, strategyDirect)
	if err != nil {
		t.Fatal(err)
	}
	result.conn.Write(payload)
	result.conn.Close()
	// Both the stalled original and winning handshake must terminate promptly.
	for i := int32(0); i < accepted.Load(); i++ {
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Fatal("losing handshake leaked")
		}
	}
	if application.Load() != 1 {
		t.Fatalf("application request delivered %d times", application.Load())
	}
	if accepted.Load() > 4 {
		t.Fatal("handshake budget exceeded")
	}
}
