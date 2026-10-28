package datapath

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseTelegramSOCKSLink(t *testing.T) {
	config, ok := parseProxyURL("tg://socks?server=127.0.0.1&port=1080&user=alice&pass=secret")
	if !ok || config.Host != "127.0.0.1" || config.Port != 1080 || config.Username != "alice" || config.Password != "secret" {
		t.Fatalf("unexpected config: %#v ok=%v", config, ok)
	}
	if _, ok := parseProxyURL("tg://proxy?server=127.0.0.1&port=443&secret=abc"); ok {
		t.Fatal("MTProxy link was incorrectly treated as a generic SOCKS proxy")
	}
}

func TestSOCKS5ConnectHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		greeting := make([]byte, 3)
		if _, readErr := io.ReadFull(connection, greeting); readErr != nil {
			serverDone <- readErr
			return
		}
		_, _ = connection.Write([]byte{0x05, 0x00})
		request := make([]byte, 10)
		if _, readErr := io.ReadFull(connection, request); readErr != nil {
			serverDone <- readErr
			return
		}
		_, _ = connection.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 80})
		payload := make([]byte, 4)
		_, readErr := io.ReadFull(connection, payload)
		if readErr == nil && string(payload) != "PING" {
			readErr = io.ErrUnexpectedEOF
		}
		serverDone <- readErr
	}()

	host, portValue, _ := net.SplitHostPort(listener.Addr().String())
	port := uint16(0)
	_, _ = fmt.Sscanf(portValue, "%d", &port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := dialSOCKS5(ctx, &net.Dialer{}, proxyConfig{Host: host, Port: port}, net.ParseIP("1.2.3.4"), 443)
	if err != nil {
		t.Fatal(err)
	}
	_, err = connection.Write([]byte("PING"))
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
