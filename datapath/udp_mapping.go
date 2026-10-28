package datapath

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"sync"
	"syscall"
	"time"
)

// Keep one protected local UDP port per application socket and address family.
// Connected kernel sockets still demultiplex replies by the exact remote tuple.
// Anchors are bounded and expire after the last flow, without a polling loop.
type udpMappingPool struct {
	mu      sync.Mutex
	dialer  *net.Dialer
	entries map[string]*udpMapping
	closed  bool
}
type udpMapping struct {
	anchor net.PacketConn
	port   int
	conns  map[*mappedUDPConn]bool
	idle   *time.Timer
}
type mappedUDPConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *mappedUDPConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
func newUDPMappingPool(d *net.Dialer) *udpMappingPool {
	return &udpMappingPool{dialer: d, entries: make(map[string]*udpMapping)}
}
func (p *udpMappingPool) control(network, address string, raw syscall.RawConn) error {
	// Fail closed before enabling port sharing: preserve the selected Network.
	if p.dialer.Control != nil {
		if err := p.dialer.Control(network, address, raw); err != nil {
			return err
		}
	}
	var err error
	if e := raw.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err == nil {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	}); e != nil {
		return e
	}
	return err
}
func (p *udpMappingPool) has(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[key+"/udp4"] != nil || p.entries[key+"/udp6"] != nil
}
func (p *udpMappingPool) dial(key string, ip net.IP, port uint16) (net.Conn, error) {
	family, local := "udp4", "0.0.0.0:0"
	if ip.To4() == nil {
		family, local = "udp6", "[::]:0"
	}
	key += "/" + family
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	entry := p.entries[key]
	if entry == nil {
		if len(p.entries) >= 128 {
			return nil, fmt.Errorf("UDP source mapping limit")
		}
		lc := net.ListenConfig{Control: p.control}
		anchor, err := lc.ListenPacket(context.Background(), family, local)
		if err != nil {
			return nil, err
		}
		entry = &udpMapping{anchor: anchor, port: anchor.LocalAddr().(*net.UDPAddr).Port, conns: make(map[*mappedUDPConn]bool)}
		p.entries[key] = entry
	}
	if entry.idle != nil {
		entry.idle.Stop()
		entry.idle = nil
	}
	d := *p.dialer
	d.Control = p.control
	d.ControlContext = nil
	d.LocalAddr = &net.UDPAddr{Port: entry.port}
	c, err := d.Dial(family, net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	if err != nil {
		p.scheduleIdleLocked(key, entry)
		return nil, err
	}
	wrapped := &mappedUDPConn{Conn: c}
	wrapped.release = func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(entry.conns, wrapped)
		p.scheduleIdleLocked(key, entry)
	}
	entry.conns[wrapped] = true
	return wrapped, nil
}
func (p *udpMappingPool) scheduleIdleLocked(key string, e *udpMapping) {
	if len(e.conns) != 0 || p.entries[key] != e || e.idle != nil {
		return
	}
	e.idle = time.AfterFunc(75*time.Second, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.entries[key] == e && len(e.conns) == 0 {
			delete(p.entries, key)
			e.anchor.Close()
		}
	})
}
func (p *udpMappingPool) reset(closePool bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if closePool {
		p.closed = true
	}
	for _, e := range p.entries {
		if e.idle != nil {
			e.idle.Stop()
		}
		e.anchor.Close()
		// Close underlying sockets directly; wrapper release takes this mutex.
		for c := range e.conns {
			c.Conn.Close()
		}
	}
	p.entries = make(map[string]*udpMapping)
}
