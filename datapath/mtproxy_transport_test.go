package datapath

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/mtproxy"
	"github.com/gotd/td/mtproxy/obfuscated2"
)

func TestMTProxyFrameRoundTrip(t *testing.T) {
	for _, tag := range []byte{0xef, 0xee, 0xdd} {
		for _, size := range []int{40, 508, 512, 4096} {
			body := make([]byte, size)
			body[0] = 1
			// Valid encrypted envelope requires (length-24)%16==0 for DD.
			if tag == 0xdd {
				body = body[:24+(len(body)-24)/16*16]
			}
			var buf bytes.Buffer
			sent := mtFrame{body: body, requestAck: true}
			if err := writeMTFrame(&buf, tag, sent); err != nil {
				t.Fatal(err)
			}
			got, err := readMTFrame(&oneByteReader{&buf}, tag, false)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.body, body) || !got.requestAck {
				t.Fatalf("tag=%x lost payload or ACK request", tag)
			}
		}
		var buf bytes.Buffer
		ack := []byte{1, 2, 3, 0x84}
		if err := writeMTFrame(&buf, tag, mtFrame{ack: ack}); err != nil {
			t.Fatal(err)
		}
		got, err := readMTFrame(&buf, tag, true)
		if err != nil || !bytes.Equal(got.ack, ack) {
			t.Fatalf("tag=%x quickACK: %v %+v", tag, err, got)
		}
	}
}

type oneByteReader struct{ io.Reader }

func (r *oneByteReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

func TestMTProxyPadding(t *testing.T) {
	plain, _, _ := telegramReqPQ()
	for pad := 0; pad < 16; pad++ {
		for _, body := range [][]byte{plain[2:], append([]byte{1}, make([]byte, 39)...)} {
			padded := append(append([]byte(nil), body...), make([]byte, pad)...)
			got, err := stripMTPadding(padded)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("padding=%d len=%d err=%v", pad, len(body), err)
			}
		}
	}
	if _, err := stripMTPadding(make([]byte, 8)); err == nil {
		t.Fatal("short malformed body accepted")
	}
	for _, tag := range []byte{0xee, 0xdd} {
		var head [4]byte
		binary.LittleEndian.PutUint32(head[:], maxMTProtoFrame+1)
		if _, err := readMTFrame(bytes.NewReader(head[:]), tag, false); err == nil {
			t.Fatal("oversized frame accepted")
		}
	}
}

func TestMTProxyUnwrapAndroidObfuscation(t *testing.T) {
	for _, tag := range []byte{0xef, 0xee, 0xdd} {
		client, server := net.Pipe()
		client.SetDeadline(time.Now().Add(3 * time.Second))
		server.SetDeadline(time.Now().Add(3 * time.Second))
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			head := make([]byte, 64)
			if _, err := io.ReadFull(server, head); err != nil {
				done <- err
				return
			}
			local, detected, err := unwrapLocalMTProto(server, head)
			if err != nil {
				done <- err
				return
			}
			if detected != tag {
				done <- io.ErrUnexpectedEOF
				return
			}
			for i := 0; i < 2; i++ {
				frame, err := readMTFrame(local, tag, false)
				if err != nil {
					done <- err
					return
				}
				frame.requestAck = false
				if err = writeMTFrame(local, tag, frame); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		obfs := obfuscated2.NewObfuscated2(rand.Reader, client)
		if err := obfs.Handshake([4]byte{tag, tag, tag, tag}, 0, mtproxy.Secret{}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			body, _, _ := telegramReqPQ()
			if err := writeMTFrame(obfs, tag, mtFrame{body: body[2:]}); err != nil {
				t.Fatal(err)
			}
			got, err := readMTFrame(obfs, tag, true)
			if err != nil || !bytes.Equal(got.body, body[2:]) {
				t.Fatalf("tag=%x continuous CTR roundtrip failed %v", tag, err)
			}
		}
		client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestMTProxyParserSafety(t *testing.T) {
	secret := "ee00112233445566778899aabbccddeeff6578616d706c652e636f6d"
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.168.1.1", "[::1]", "224.0.0.1"} {
		if _, ok := parseMTProxy("tg://proxy?server=" + host + "&port=443&secret=" + secret); ok {
			t.Fatalf("private endpoint accepted %s", host)
		}
	}
	if _, ok := parseMTProxy("tg://proxy?server=example.com&port=443&secret=" + secret); !ok {
		t.Fatal("valid EE proxy rejected")
	}
	if _, ok := parseMTProxy("tg://proxy?server=example.com&port=443&secret=dd00112233445566778899aabbccddeeff"); ok {
		t.Fatal("unsupported non-TLS proxy accepted")
	}
}
