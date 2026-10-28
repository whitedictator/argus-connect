package datapath

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSignalReflectorPreservesOpaqueBytesAfterDialCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tls-tunnel" || r.Header.Get("X-Signal-Host") != "grpc.chat.signal.org" {
			http.Error(w, "wrong destination", http.StatusBadRequest)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(256 * 1024)
		for {
			kind, b, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if kind != websocket.MessageBinary {
				return
			}
			if err := ws.Write(r.Context(), kind, b); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	c, err := upgradeSignalReflector(ctx, lifetime, server.Client(), "ws"+strings.TrimPrefix(server.URL, "http")+"/tls-tunnel", "grpc.chat.signal.org")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x16, 0x03, 0x03, 0, 0xff}, 15000)
	done := make(chan error, 1)
	go func() { _, err := c.Write(want); done <- err }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("opaque stream rewritten")
	}
	stop()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("engine cancellation did not close stream")
	}
}

func TestSignalReflectorRejectsUnrelatedTarget(t *testing.T) {
	if _, err := upgradeSignalReflector(context.Background(), context.Background(), http.DefaultClient, "http://127.0.0.1:1", "www.google.com"); err == nil {
		t.Fatal("unrelated target accepted")
	}
}
