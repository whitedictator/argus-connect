package datapath

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// Only a complete, standalone ClientHello can be replayed during the handshake.
// TLS 1.3 early_data, coalesced application records and malformed extensions
// stay on the existing path. No application bytes are read or duplicated here.
func safeYouTubeHello(b []byte, host string) bool {
	if !strings.HasSuffix(host, ".googlevideo.com") || len(b) < 44 || b[0] != 22 || b[5] != 1 || 5+int(binary.BigEndian.Uint16(b[3:5])) != len(b) {
		return false
	}
	if 9+(int(b[6])<<16|int(b[7])<<8|int(b[8])) != len(b) {
		return false
	}
	p := 43
	take := func(n int) bool {
		if n < 0 || p+n > len(b) {
			return false
		}
		p += n
		return true
	}
	n := int(b[p])
	p++
	if !take(n) || p+2 > len(b) {
		return false
	}
	n = int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if !take(n) || p >= len(b) {
		return false
	}
	n = int(b[p])
	p++
	if !take(n) || p+2 > len(b) {
		return false
	}
	end := p + 2 + int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if end != len(b) {
		return false
	}
	for p < end {
		if p+4 > end {
			return false
		}
		kind := binary.BigEndian.Uint16(b[p:])
		n = int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if !take(n) || kind == 42 {
			return false
		}
	}
	return p == end
}

type youtubeHandshake struct {
	conn     net.Conn
	first    []byte
	strategy strategyID
}

// Four races at most, each with four staggered attempts; normal quick routes
// finish before an additional socket opens. Always the same remote IP and port.
var youtubeHandshakeSlots = make(chan struct{}, 4)

func raceYouTubeHello(parent context.Context, dialer *net.Dialer, initial net.Conn, hello []byte, preferred strategyID) (youtubeHandshake, error) {
	ctx, cancel := context.WithTimeout(parent, 2300*time.Millisecond)
	defer cancel()
	methods := []strategyID{preferred, strategyComboFast, strategyComboMid, strategyTLSRecord}
	results := make(chan youtubeHandshake)
	failed := make(chan struct{}, len(methods))
	for i, method := range methods {
		go func(i int, method strategyID) {
			if i > 0 {
				timer := time.NewTimer(time.Duration(i) * 250 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
			c := initial
			var err error
			if i > 0 {
				c, err = dialer.DialContext(ctx, "tcp", initial.RemoteAddr().String())
			}
			if err != nil {
				failed <- struct{}{}
				return
			}
			stop := context.AfterFunc(ctx, func() { c.Close() })
			c.SetDeadline(time.Now().Add(1200 * time.Millisecond))
			err = applyStrategy(c, hello, method)
			var first []byte
			if err == nil {
				first, err = readServerHello(c)
			}
			stop()
			if err != nil {
				c.Close()
				failed <- struct{}{}
				return
			}
			c.SetDeadline(time.Time{})
			select {
			case results <- youtubeHandshake{c, first, method}:
			case <-ctx.Done():
				c.Close()
			}
		}(i, method)
	}
	for failures := 0; failures < len(methods); {
		select {
		case result := <-results:
			return result, nil
		case <-failed:
			failures++
		case <-ctx.Done():
			return youtubeHandshake{}, ctx.Err()
		}
	}
	return youtubeHandshake{}, errors.New("YouTube TLS attempts unanswered")
}
func readServerHello(c net.Conn) ([]byte, error) {
	h := make([]byte, 5)
	if _, e := io.ReadFull(c, h); e != nil {
		return nil, e
	}
	n := int(binary.BigEndian.Uint16(h[3:]))
	if h[0] != 22 || n < 42 || n > 18432 {
		return nil, errors.New("not ServerHello")
	}
	b := append(h, make([]byte, n)...)
	if _, e := io.ReadFull(c, b[5:]); e != nil {
		return nil, e
	}
	if b[5] != 2 || (int(b[6])<<16|int(b[7])<<8|int(b[8]))+4 > n {
		return nil, errors.New("invalid ServerHello")
	}
	return b, nil
}
