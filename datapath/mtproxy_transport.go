package datapath

// Only the public MTProto transport envelope is adapted. The encrypted MTProto
// message (auth key id, message key and ciphertext) is never decrypted/modified.
// https://core.telegram.org/mtproto/mtproto-transports
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gotd/td/mtproxy"
	"github.com/gotd/td/mtproxy/faketls"
	"github.com/gotd/td/mtproxy/obfuscated2"
)

const maxMTProtoFrame = 4 * 1024 * 1024

type mtProxyStream struct {
	net.Conn
	stream io.ReadWriter
}

func (c *mtProxyStream) Read(p []byte) (int, error)  { return c.stream.Read(p) }
func (c *mtProxyStream) Write(p []byte) (int, error) { return c.stream.Write(p) }

// Explicit opt-in relay; preserve encrypted MTProto envelopes and DC affinity.
func openConfiguredMTProxy(ctx context.Context, dialer *net.Dialer, address string, config mtProxyConfig, dc int) (net.Conn, error) {
	if config.wsDC == 0 {
		return openMTProxy(ctx, dialer, address, config.secret, dc)
	}
	if dc != config.wsDC && dc != -config.wsDC {
		return nil, fmt.Errorf("WSS relay DC mismatch")
	}
	c, err := openTelegramWebSocketStrategy(ctx, dialer, config.host, address, strategyDirect)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(6 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	obfs := obfuscated2.NewObfuscated2(rand.Reader, c)
	if err = obfs.Handshake([4]byte{0xdd, 0xdd, 0xdd, 0xdd}, dc, mtproxy.Secret{}); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &mtProxyStream{c, obfs}, nil
}

func openMTProxy(ctx context.Context, dialer *net.Dialer, address string, secret mtproxy.Secret, dc int) (net.Conn, error) {
	c, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("TCP: %w", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	ftls := faketls.NewFakeTLS(rand.Reader, c)
	tag := [4]byte{0xdd, 0xdd, 0xdd, 0xdd}
	if err = ftls.Handshake(tag, dc, secret); err != nil {
		c.Close()
		return nil, fmt.Errorf("FakeTLS: %w", err)
	}
	obfs := obfuscated2.NewObfuscated2(rand.Reader, ftls)
	if err = obfs.Handshake(tag, dc, secret); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &mtProxyStream{c, obfs}, nil
}

type localMTProtoStream struct {
	net.Conn
	reader  io.Reader
	encrypt cipher.Stream
}

func (c *localMTProtoStream) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *localMTProtoStream) Write(p []byte) (int, error) {
	if c.encrypt == nil {
		return c.Conn.Write(p)
	}
	buf := make([]byte, len(p))
	c.encrypt.XORKeyStream(buf, p)
	if err := writeAll(c.Conn, buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// first must contain the complete initial header. For ordinary Android direct
// connections DC bytes are RANDOM; select DC from the known destination, not
// from bytes 60..62. An unknown/proxy-secret header is rejected, never guessed.
func unwrapLocalMTProto(c net.Conn, first []byte) (net.Conn, byte, error) {
	if len(first) > 0 && first[0] == 0xef {
		return &localMTProtoStream{Conn: c, reader: io.MultiReader(bytes.NewReader(first[1:]), c)}, 0xef, nil
	}
	if len(first) >= 4 {
		tag := binary.LittleEndian.Uint32(first)
		if tag == 0xeeeeeeee || tag == 0xdddddddd {
			return &localMTProtoStream{Conn: c, reader: io.MultiReader(bytes.NewReader(first[4:]), c)}, byte(tag), nil
		}
	}
	if len(first) < 64 {
		return nil, 0, fmt.Errorf("incomplete MTProto header")
	}
	head := first[:64]
	inBlock, _ := aes.NewCipher(head[8:40])
	decrypt := cipher.NewCTR(inBlock, head[40:56])
	clear := make([]byte, 64)
	decrypt.XORKeyStream(clear, head)
	tag := binary.LittleEndian.Uint32(clear[56:60])
	if tag != 0xefefefef && tag != 0xeeeeeeee && tag != 0xdddddddd {
		return nil, 0, fmt.Errorf("not unkeyed MTProto obfuscation")
	}
	reverse := make([]byte, 48)
	for i := range reverse {
		reverse[i] = head[55-i]
	}
	outBlock, _ := aes.NewCipher(reverse[:32])
	encrypt := cipher.NewCTR(outBlock, reverse[32:])
	r := &cipher.StreamReader{S: decrypt, R: io.MultiReader(bytes.NewReader(first[64:]), c)}
	return &localMTProtoStream{Conn: c, reader: r, encrypt: encrypt}, byte(tag), nil
}

type mtFrame struct {
	body       []byte
	requestAck bool
	ack        []byte
}

func readMTFrame(r io.Reader, tag byte, fromServer bool) (mtFrame, error) {
	var f mtFrame
	var head [4]byte
	var length uint32
	if tag == 0xef {
		if _, err := io.ReadFull(r, head[:1]); err != nil {
			return f, err
		}
		if fromServer && head[0]&0x80 != 0 {
			if _, err := io.ReadFull(r, head[1:]); err != nil {
				return f, err
			}
			f.ack = []byte{head[3], head[2], head[1], head[0]}
			return f, nil
		}
		f.requestAck = head[0]&0x80 != 0
		length = uint32(head[0] & 0x7f)
		if length == 0x7f {
			if _, err := io.ReadFull(r, head[:3]); err != nil {
				return f, err
			}
			length = uint32(head[0]) | uint32(head[1])<<8 | uint32(head[2])<<16
		}
		length *= 4
	} else {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return f, err
		}
		length = binary.LittleEndian.Uint32(head[:])
		if fromServer && tag == 0xee && length&0x80000000 != 0 {
			f.ack = append([]byte(nil), head[:]...)
			return f, nil
		}
		f.requestAck = length&0x80000000 != 0
		length &= 0x7fffffff
	}
	if length < 4 || length > maxMTProtoFrame {
		return f, fmt.Errorf("invalid MTProto frame length %d", length)
	}
	f.body = make([]byte, length)
	if _, err := io.ReadFull(r, f.body); err != nil {
		return f, err
	}
	if tag == 0xdd {
		if fromServer && len(f.body) >= 8 && len(f.body) <= 16 && binary.LittleEndian.Uint32(f.body) == 0xffffffff {
			f.ack = append([]byte(nil), f.body[4:8]...)
			f.body = nil
			return f, nil
		}
		body, err := stripMTPadding(f.body)
		if err != nil {
			return f, err
		}
		f.body = body
	}
	return f, nil
}

func stripMTPadding(body []byte) ([]byte, error) {
	n := len(body)
	if n < 20 {
		if n >= 4 && int32(binary.LittleEndian.Uint32(body)) < 0 {
			return body[:4], nil
		}
		return nil, fmt.Errorf("short MTProto body")
	}
	if binary.LittleEndian.Uint64(body[:8]) == 0 {
		length := uint64(binary.LittleEndian.Uint32(body[16:20])) + 20
		if length > uint64(n) || uint64(n)-length > 15 || length%4 != 0 {
			return nil, fmt.Errorf("invalid unencrypted envelope")
		}
		return body[:int(length)], nil
	}
	// Encrypted MTProto envelope: 8-byte auth id + 16-byte msg key + N*16 ciphertext.
	if n < 40 {
		return nil, fmt.Errorf("short encrypted envelope")
	}
	length := 24 + (n-24)/16*16
	return body[:length], nil
}

func writeMTFrame(w io.Writer, tag byte, f mtFrame) error {
	var head [4]byte
	if len(f.ack) > 0 {
		if len(f.ack) != 4 {
			return fmt.Errorf("bad quick ack")
		}
		switch tag {
		case 0xef:
			return writeAll(w, []byte{f.ack[3], f.ack[2], f.ack[1], f.ack[0]})
		case 0xee:
			return writeAll(w, f.ack)
		case 0xdd:
			binary.LittleEndian.PutUint32(head[:], 8)
			if err := writeAll(w, head[:]); err != nil {
				return err
			}
			return writeAll(w, append([]byte{255, 255, 255, 255}, f.ack...))
		}
	}
	n := len(f.body)
	if n < 4 || n > maxMTProtoFrame || n%4 != 0 {
		return fmt.Errorf("unaligned/oversize MTProto body")
	}
	if tag == 0xef {
		words := n / 4
		if words < 127 {
			head[0] = byte(words)
			if f.requestAck {
				head[0] |= 0x80
			}
			if err := writeAll(w, head[:1]); err != nil {
				return err
			}
		} else {
			head = [4]byte{0x7f, byte(words), byte(words >> 8), byte(words >> 16)}
			if f.requestAck {
				head[0] |= 0x80
			}
			if err := writeAll(w, head[:]); err != nil {
				return err
			}
		}
	} else {
		length := uint32(n)
		if f.requestAck {
			length |= 0x80000000
		}
		binary.LittleEndian.PutUint32(head[:], length)
		if err := writeAll(w, head[:]); err != nil {
			return err
		}
	}
	// Zero padding is valid for DD; ciphertext itself is never changed.
	return writeAll(w, f.body)
}

func verifyMTProxyConnection(c net.Conn) error {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	payload, nonce, err := telegramReqPQ()
	if err != nil {
		return err
	}
	if err = writeMTFrame(c, 0xdd, mtFrame{body: payload[2:]}); err != nil {
		return err
	}
	reply, err := readMTFrame(c, 0xdd, true)
	if err != nil {
		return err
	}
	b := reply.body
	if len(b) < 40 || binary.LittleEndian.Uint64(b) != 0 || binary.LittleEndian.Uint32(b[20:24]) != 0x05162463 || !bytes.Equal(b[24:40], nonce) {
		return fmt.Errorf("MTProto nonce mismatch")
	}
	return nil
}
