package datapath

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	telegramWebSocketPath     = "/apiws"
	telegramWebSocketProtocol = "binary"
	telegramWebSocketGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maximumWebSocketFrame     = 16 * 1024 * 1024
)

var telegramWebNames = []string{"", "pluto", "venus", "aurora", "vesta", "flora"}

func telegramDCNumber(destination net.IP) int {
	wanted := destination.String()
	for index, dc := range telegramDCs {
		for _, group := range [][]string{dc.main, dc.media} {
			for _, value := range group {
				if net.ParseIP(value).String() == wanted {
					return index + 1
				}
			}
		}
	}
	return 0
}

// dialTelegramWebSocket opens Telegram's own documented WSS transport. It is
// a local fallback, not an Argus relay: TLS still terminates at Telegram and
// the selected physical Android Network remains the only upstream.
func dialTelegramWebSocket(
	dialer *net.Dialer,
	policy *lightPolicy,
	destination net.IP,
) (net.Conn, string, error) {
	dc := telegramDCNumber(destination)
	if dc < 1 || dc >= len(telegramWebNames) {
		return nil, "", fmt.Errorf("Telegram WSS DC is unknown for %s", destination)
	}
	host := telegramWebNames[dc] + ".web.telegram.org"
	addresses := encryptedLookup(policy.doh, host)
	if len(addresses) == 0 {
		// This is only a last resolver fallback. Normally the same encrypted
		// resolver used by LIGHT supplies the exact A/AAAA set.
		addresses = []net.IP{nil}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	type result struct {
		connection net.Conn
		err        error
	}
	// Unbuffered delivery guarantees losing race connections observe ctx.Done
	// and close instead of being queued after the winner has been returned.
	results := make(chan result)
	for _, address := range addresses {
		address := address
		go func() {
			target := net.JoinHostPort(host, "443")
			if address != nil {
				target = net.JoinHostPort(address.String(), "443")
			}
			connection, err := openTelegramWebSocket(ctx, dialer, host, target)
			select {
			case results <- result{connection, err}:
			case <-ctx.Done():
				if connection != nil {
					_ = connection.Close()
				}
			}
		}()
	}
	var last error
	for range addresses {
		select {
		case candidate := <-results:
			if candidate.err == nil {
				cancel()
				return candidate.connection, "telegram-wss-dc" + strconv.Itoa(dc), nil
			}
			last = candidate.err
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return nil, "", last
		}
	}
	return nil, "", last
}

func encryptedLookup(resolver *dohResolver, host string) []net.IP {
	if resolver == nil {
		return nil
	}
	result := make([]net.IP, 0, 4)
	seen := make(map[string]bool)
	for _, queryType := range []uint16{1, 28} { // A, AAAA
		query, err := makeDNSQuery(host, queryType)
		if err != nil {
			continue
		}
		response, err := resolver.resolve(query)
		if err != nil {
			continue
		}
		_, addresses, err := parseDNSAnswer(response)
		if err != nil {
			continue
		}
		for _, address := range addresses {
			key := address.String()
			if address.IsGlobalUnicast() && !seen[key] {
				seen[key] = true
				result = append(result, address)
			}
		}
	}
	return result
}

func makeDNSQuery(host string, queryType uint16) ([]byte, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	labels := strings.Split(host, ".")
	query := make([]byte, 12, 512)
	if _, err := rand.Read(query[:2]); err != nil {
		return nil, err
	}
	query[2] = 1 // recursion desired
	binary.BigEndian.PutUint16(query[4:6], 1)
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS label")
		}
		query = append(query, byte(len(label)))
		query = append(query, label...)
	}
	query = append(query, 0, byte(queryType>>8), byte(queryType), 0, 1)
	return query, nil
}

func openTelegramWebSocket(
	ctx context.Context,
	dialer *net.Dialer,
	host string,
	target string,
) (net.Conn, error) {
	return openTelegramWebSocketStrategy(ctx, dialer, host, target, strategyDirect)
}

type firstWriteStrategyConn struct {
	net.Conn
	strategy strategyID
	wrote    bool
}

func (c *firstWriteStrategyConn) Write(p []byte) (int, error) {
	if c.wrote {
		return c.Conn.Write(p)
	}
	c.wrote = true
	if err := applyStrategy(c.Conn, p, c.strategy); err != nil {
		return 0, err
	}
	return len(p), nil
}

func openTelegramWebSocketStrategy(ctx context.Context, dialer *net.Dialer, host, target string, strategy strategyID) (net.Conn, error) {
	raw, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("tcp: %w", err)
	}
	connection := tls.Client(&firstWriteStrategyConn{Conn: raw, strategy: strategy}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
		NextProtos: []string{"http/1.1"},
	})
	if err := connection.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = connection.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := "GET " + telegramWebSocketPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: " + telegramWebSocketProtocol + "\r\n\r\n"
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(connection, request); err != nil {
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		_ = connection.Close()
		return nil, fmt.Errorf("Telegram WSS upgrade rejected: %s", strings.TrimSpace(status))
	}
	headers := make(map[string]string)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			_ = connection.Close()
			return nil, readErr
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if found {
			headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	digest := sha1.Sum([]byte(key + telegramWebSocketGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(digest[:])
	if !strings.EqualFold(headers["upgrade"], "websocket") ||
		headers["sec-websocket-accept"] != expectedAccept ||
		!strings.EqualFold(headers["sec-websocket-protocol"], telegramWebSocketProtocol) {
		_ = connection.Close()
		return nil, fmt.Errorf("Telegram WSS returned invalid upgrade headers")
	}
	_ = connection.SetDeadline(time.Time{})
	return &telegramWebSocketConn{Conn: connection, reader: reader}, nil
}

// telegramWebSocketConn exposes RFC 6455 binary messages as one duplex byte
// stream, exactly as Telegram defines for MTProto-over-WebSocket.
type telegramWebSocketConn struct {
	net.Conn
	reader  *bufio.Reader
	readMu  sync.Mutex
	writeMu sync.Mutex
	pending []byte
}

func (c *telegramWebSocketConn) Read(destination []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		first, err := c.reader.ReadByte()
		if err != nil {
			return 0, err
		}
		second, err := c.reader.ReadByte()
		if err != nil {
			return 0, err
		}
		opcode := first & 0x0f
		length := uint64(second & 0x7f)
		switch length {
		case 126:
			var size [2]byte
			if _, err = io.ReadFull(c.reader, size[:]); err != nil {
				return 0, err
			}
			length = uint64(binary.BigEndian.Uint16(size[:]))
		case 127:
			var size [8]byte
			if _, err = io.ReadFull(c.reader, size[:]); err != nil {
				return 0, err
			}
			length = binary.BigEndian.Uint64(size[:])
		}
		if length > maximumWebSocketFrame {
			return 0, fmt.Errorf("Telegram WSS frame too large: %d", length)
		}
		var mask [4]byte
		if second&0x80 != 0 {
			if _, err = io.ReadFull(c.reader, mask[:]); err != nil {
				return 0, err
			}
		}
		payload := make([]byte, int(length))
		if _, err = io.ReadFull(c.reader, payload); err != nil {
			return 0, err
		}
		if second&0x80 != 0 {
			for index := range payload {
				payload[index] ^= mask[index%4]
			}
		}
		switch opcode {
		case 0, 2: // continuation or binary
			c.pending = payload
		case 8:
			return 0, io.EOF
		case 9:
			if err := c.writeFrame(10, payload); err != nil {
				return 0, err
			}
		case 10:
			continue
		default:
			return 0, fmt.Errorf("unexpected Telegram WSS opcode %d", opcode)
		}
	}
	count := copy(destination, c.pending)
	c.pending = c.pending[count:]
	return count, nil
}

func (c *telegramWebSocketConn) Write(payload []byte) (int, error) {
	if err := c.writeFrame(2, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (c *telegramWebSocketConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(payload) > maximumWebSocketFrame {
		return fmt.Errorf("Telegram WSS write too large: %d", len(payload))
	}
	frame := make([]byte, 0, len(payload)+14)
	frame = append(frame, 0x80|opcode)
	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(frame[len(frame)-2:], uint16(len(payload)))
	default:
		frame = append(frame, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(frame[len(frame)-8:], uint64(len(payload)))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame = append(frame, mask[:]...)
	start := len(frame)
	frame = append(frame, payload...)
	for index := start; index < len(frame); index++ {
		frame[index] ^= mask[(index-start)%4]
	}
	return writeAll(c.Conn, frame)
}

// probeTelegramObfuscated proves more than a successful HTTP upgrade: it runs
// a real req_pq/resPQ exchange inside Telegram's required obfuscated
// intermediate transport and validates the random nonce in the reply.
func probeTelegramObfuscated(connection net.Conn, dc int, payload, nonce []byte) bool {
	if len(payload) < 3 || dc < 1 || dc > 5 {
		return false
	}
	header := make([]byte, 64)
	for {
		if _, err := rand.Read(header); err != nil {
			return false
		}
		first := binary.LittleEndian.Uint32(header[:4])
		second := binary.LittleEndian.Uint32(header[4:8])
		if header[0] != 0xef && second != 0 &&
			first != 0x44414548 && first != 0x54534f50 &&
			first != 0x20544547 && first != 0x4954504f &&
			first != 0xdddddddd && first != 0xeeeeeeee && first != 0x02010316 {
			break
		}
	}
	binary.LittleEndian.PutUint32(header[56:60], 0xeeeeeeee)
	binary.LittleEndian.PutUint16(header[60:62], uint16(dc))
	reversed := append([]byte(nil), header...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	outBlock, err := aes.NewCipher(header[8:40])
	if err != nil {
		return false
	}
	inBlock, err := aes.NewCipher(reversed[8:40])
	if err != nil {
		return false
	}
	outStream := cipher.NewCTR(outBlock, header[40:56])
	inStream := cipher.NewCTR(inBlock, reversed[40:56])
	encryptedHeader := make([]byte, len(header))
	outStream.XORKeyStream(encryptedHeader, header)
	copy(header[56:64], encryptedHeader[56:64])

	// telegramReqPQ returns abridged framing. WSS is initialized here with
	// intermediate framing, so keep the MTProto envelope and replace the frame.
	envelope := payload[2:]
	framed := make([]byte, 4+len(envelope))
	binary.LittleEndian.PutUint32(framed[:4], uint32(len(envelope)))
	copy(framed[4:], envelope)
	outStream.XORKeyStream(framed, framed)
	if err := writeAll(connection, append(header, framed...)); err != nil {
		return false
	}

	var encryptedLength [4]byte
	if _, err := io.ReadFull(connection, encryptedLength[:]); err != nil {
		return false
	}
	inStream.XORKeyStream(encryptedLength[:], encryptedLength[:])
	length := binary.LittleEndian.Uint32(encryptedLength[:]) & 0x7fffffff
	if length < 40 || length > 1<<20 {
		return false
	}
	response := make([]byte, int(length))
	if _, err := io.ReadFull(connection, response); err != nil {
		return false
	}
	inStream.XORKeyStream(response, response)
	if len(response) < 40 {
		return false
	}
	bodyLength := int(binary.LittleEndian.Uint32(response[16:20]))
	return bodyLength >= 20 && bodyLength <= len(response)-20 &&
		binary.LittleEndian.Uint32(response[20:24]) == 0x05162463 &&
		string(response[24:40]) == string(nonce)
}
