package datapath

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func udpEcho(t *testing.T) *net.UDPAddr {
	t.Helper()
	s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	go func() {
		b := make([]byte, 65535)
		for {
			n, addr, err := s.ReadFromUDP(b)
			if err != nil {
				return
			}
			s.WriteToUDP(b[:n], addr)
		}
	}()
	return s.LocalAddr().(*net.UDPAddr)
}
func mappingEcho(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 65535)
	n, err := c.Read(b)
	if err != nil || !bytes.Equal(b[:n], payload) {
		t.Fatalf("UDP payload/reply corrupted: %v", err)
	}
}
func TestUDPVoiceMappingPreservesPortAndReplyIsolation(t *testing.T) {
	p := newUDPMappingPool(&net.Dialer{Timeout: time.Second})
	defer p.reset(true)
	a, b := udpEcho(t), udpEcho(t)
	first, err := p.dial("client-A", a.IP, uint16(a.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := p.dial("client-A", b.IP, uint16(b.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.LocalAddr().String() != second.LocalAddr().String() {
		t.Fatal("same client changed its UDP source port across destinations")
	}
	mappingEcho(t, first, []byte{1, 2, 0, 255})
	mappingEcho(t, second, []byte{9, 8, 0, 3})
	first.Close()
	mappingEcho(t, second, []byte("other destination survives"))
	other, err := p.dial("client-B", a.IP, uint16(a.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.LocalAddr().String() == second.LocalAddr().String() {
		t.Fatal("different clients share mapping")
	}
	mappingEcho(t, other, []byte("separate client"))
	p.reset(false)
	if _, err = second.Write([]byte("must be closed")); err == nil {
		t.Fatal("network change left an old UDP socket open")
	}
	if p.has("client-A") {
		t.Fatal("network reset retained old mapping")
	}
	reopened, err := p.dial("client-A", a.IP, uint16(a.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mappingEcho(t, reopened, []byte("new network"))
	p.reset(true)
	if _, err = p.dial("client-A", a.IP, uint16(a.Port)); err == nil {
		t.Fatal("stopped engine reopened UDP mapping")
	}
}
func TestUDPMappingRetainsFailClosedControl(t *testing.T) {
	p := newUDPMappingPool(&net.Dialer{Control: (&networkSelector{}).control})
	defer p.reset(true)
	if c, err := p.dial("client", net.IPv4(127, 0, 0, 1), 1234); err == nil {
		c.Close()
		t.Fatal("mapping bypassed network binding failure")
	}
}
