package datapath

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"time"
)

type telegramEndpoint struct {
	address net.IP
	label   string
}

type telegramDC struct {
	name  string
	main  []string
	media []string
}

// Bootstrap plus help.getConfig peers verified using Telegram's pinned RSA keys
// over the Fold's cellular network on 2026-09-08. Never infer DC from a /24:
// 149.154.167.41 belongs to DC2 but .91/.92 belong to DC4.
// The legacy .92 mapping is also recorded by official tdesktop's protocol logs:
// https://github.com/telegramdesktop/tdesktop/issues/16353 .
// Keep exact same-DC alternatives to avoid redirecting an account's encrypted
// session to another DC. Unknown endpoints remain unmapped, not guessed.
var telegramDCs = []telegramDC{
	{name: "dc1", main: []string{"149.154.175.50", "149.154.175.51", "149.154.175.53", "149.154.175.57", "149.154.175.55", "2001:b28:f23d:f001::a"}},
	{name: "dc2", main: []string{"149.154.167.50", "149.154.167.51", "149.154.167.41", "2001:67c:4e8:f002::a"}, media: []string{"149.154.167.151", "149.154.167.222", "2001:67c:4e8:f002::b"}},
	{name: "dc3", main: []string{"149.154.175.100", "2001:b28:f23d:f003::a"}},
	{name: "dc4", main: []string{"149.154.167.91", "149.154.167.92", "2001:67c:4e8:f004::a"}, media: []string{"149.154.165.96", "149.154.165.111", "149.154.167.43", "2001:67c:4e8:f004::b"}},
	{name: "dc5", main: []string{"91.108.56.125", "91.108.56.195", "91.108.56.177", "149.154.171.5", "2001:b28:f23f:f005::a"}},
}

func telegramProxyDC(destination net.IP) int {
	// Experimental exact media mappings: these endpoints accepted pinned-RSA
	// server DH only for -2/-4 in the Sep 14 control (final DhGen timed out).
	// LTE relays separately completed help.getConfig for DC2/DC4. Do not expand
	// this evidence to neighboring addresses or claim a verified file download.
	if destination.Equal(net.ParseIP("149.154.162.123")) {
		return -2
	}
	if destination.Equal(net.ParseIP("149.154.167.255")) {
		return -4
	}
	for index, dc := range telegramDCs {
		for _, ip := range dc.main {
			if net.ParseIP(ip).Equal(destination) {
				return index + 1
			}
		}
		for _, ip := range dc.media {
			if net.ParseIP(ip).Equal(destination) {
				return -(index + 1)
			}
		}
	}
	return 0
}

func telegramAlternates(destination net.IP) []telegramEndpoint {
	wanted := destination.String()
	for _, dc := range telegramDCs {
		for _, group := range [][]string{dc.main, dc.media} {
			matched := false
			for _, value := range group {
				if net.ParseIP(value).String() == wanted {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			alternates := make([]telegramEndpoint, 0, len(group)-1)
			for _, value := range group {
				address := net.ParseIP(value)
				if address.String() == wanted {
					continue
				}
				family := "v4-peer"
				if address.To4() == nil {
					family = "ipv6"
				}
				alternates = append(alternates, telegramEndpoint{address, "telegram-" + dc.name + "-" + family})
			}
			return alternates
		}
	}
	return nil
}

func telegramProbeTargets() []net.IP {
	result := make([]net.IP, 0, len(telegramDCs)*2)
	for _, dc := range telegramDCs {
		ipv4Added := false
		ipv6Added := false
		for _, value := range dc.main {
			address := net.ParseIP(value)
			if address.To4() != nil && !ipv4Added {
				result = append(result, address)
				ipv4Added = true
			} else if address.To4() == nil && !ipv6Added {
				result = append(result, address)
				ipv6Added = true
			}
		}
	}
	return result
}

func telegramReqPQ() ([]byte, []byte, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	body := make([]byte, 20)
	binary.LittleEndian.PutUint32(body[0:4], 0xbe7e8ef1) // req_pq_multi
	copy(body[4:], nonce)
	envelope := make([]byte, 20+len(body))
	now := time.Now()
	messageID := uint64(now.Unix())<<32 |
		(uint64(now.Nanosecond())<<32)/uint64(time.Second)
	messageID &^= 3
	binary.LittleEndian.PutUint64(envelope[8:16], messageID)
	binary.LittleEndian.PutUint32(envelope[16:20], uint32(len(body)))
	copy(envelope[20:], body)
	// Abridged transport marker followed by the envelope length in 32-bit words.
	return append([]byte{0xef, byte(len(envelope) / 4)}, envelope...), nonce, nil
}

func readTelegramResPQ(connection net.Conn, nonce []byte) bool {
	var header [1]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil || header[0]&0x80 != 0 {
		return false
	}
	words := int(header[0])
	if words == 0x7f {
		var extended [3]byte
		if _, err := io.ReadFull(connection, extended[:]); err != nil {
			return false
		}
		words = int(extended[0]) | int(extended[1])<<8 | int(extended[2])<<16
	}
	if words < 10 || words > 1<<16 {
		return false
	}
	payload := make([]byte, words*4)
	if _, err := io.ReadFull(connection, payload); err != nil || len(payload) < 40 {
		return false
	}
	bodyLength := int(binary.LittleEndian.Uint32(payload[16:20]))
	return bodyLength >= 20 && bodyLength <= len(payload)-20 &&
		binary.LittleEndian.Uint32(payload[20:24]) == 0x05162463 && // resPQ
		bytes.Equal(payload[24:40], nonce)
}
