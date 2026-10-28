package datapath

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maximumProxyCount = 12

type proxyConfig struct {
	Host     string
	Port     uint16
	Username string
	Password string
}

type proxyPool struct {
	mu        sync.RWMutex
	configs   []proxyConfig
	preferred int
}

func (p *proxyPool) configure(encoded string) {
	configs := make([]proxyConfig, 0, maximumProxyCount)
	seen := make(map[string]bool)
	for _, line := range strings.Fields(encoded) {
		config, ok := parseProxyURL(strings.TrimSpace(line))
		if !ok {
			continue
		}
		key := net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))) + "\x00" + config.Username
		if seen[key] {
			continue
		}
		seen[key] = true
		configs = append(configs, config)
		if len(configs) == maximumProxyCount {
			break
		}
	}
	p.mu.Lock()
	p.configs = configs
	p.preferred = 0
	p.mu.Unlock()
}

func parseProxyURL(value string) (proxyConfig, bool) {
	parsed, err := url.Parse(value)
	if err != nil {
		return proxyConfig{}, false
	}
	if strings.EqualFold(parsed.Scheme, "tg") && strings.EqualFold(parsed.Host, "socks") {
		query := parsed.Query()
		parsed = &url.URL{
			Scheme: "socks5",
			Host:   net.JoinHostPort(query.Get("server"), query.Get("port")),
		}
		if query.Get("user") != "" || query.Get("pass") != "" {
			parsed.User = url.UserPassword(query.Get("user"), query.Get("pass"))
		}
	}
	if !strings.EqualFold(parsed.Scheme, "socks5") && !strings.EqualFold(parsed.Scheme, "socks5h") {
		return proxyConfig{}, false
	}
	host := parsed.Hostname()
	portValue, err := strconv.Atoi(parsed.Port())
	if err != nil || host == "" || portValue < 1 || portValue > 65535 || strings.ContainsAny(host, "\r\n\x00") {
		return proxyConfig{}, false
	}
	config := proxyConfig{Host: host, Port: uint16(portValue)}
	if parsed.User != nil {
		config.Username = parsed.User.Username()
		config.Password, _ = parsed.User.Password()
		if len(config.Username) > 255 || len(config.Password) > 255 {
			return proxyConfig{}, false
		}
	}
	return config, true
}

func (p *proxyPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.configs)
}

func (p *proxyPool) dial(
	dialer *net.Dialer,
	destination net.IP,
	port uint16,
) (net.Conn, string, error) {
	p.mu.RLock()
	configs := append([]proxyConfig(nil), p.configs...)
	start := p.preferred
	p.mu.RUnlock()
	if len(configs) == 0 {
		return nil, "", fmt.Errorf("proxy pool is empty")
	}
	var last error
	for offset := range configs {
		index := (start + offset) % len(configs)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		connection, err := dialSOCKS5(ctx, dialer, configs[index], destination, port)
		cancel()
		if err == nil {
			p.mu.Lock()
			p.preferred = index
			p.mu.Unlock()
			return connection, fmt.Sprintf("socks5-%d", index+1), nil
		}
		last = err
	}
	return nil, "", last
}

func (p *proxyPool) statusJSON() json.RawMessage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	preferred := 0
	if len(p.configs) > 0 {
		preferred = p.preferred + 1
	}
	encoded, _ := json.Marshal(struct {
		Count     int `json:"count"`
		Preferred int `json:"preferred,omitempty"`
	}{len(p.configs), preferred})
	return encoded
}

func (p *proxyPool) report(route string, success bool) {
	if success || !strings.HasPrefix(route, "socks5-") {
		return
	}
	index, err := strconv.Atoi(strings.TrimPrefix(route, "socks5-"))
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	index--
	if len(p.configs) > 1 && index >= 0 && index < len(p.configs) && p.preferred == index {
		p.preferred = (index + 1) % len(p.configs)
	}
}

func dialSOCKS5(
	ctx context.Context,
	dialer *net.Dialer,
	proxy proxyConfig,
	destination net.IP,
	port uint16,
) (net.Conn, error) {
	connection, err := dialer.DialContext(
		ctx,
		"tcp",
		net.JoinHostPort(proxy.Host, strconv.Itoa(int(proxy.Port))),
	)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		_ = connection.Close()
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if ok {
		_ = connection.SetDeadline(deadline)
	}
	methods := []byte{0x00}
	if proxy.Username != "" || proxy.Password != "" {
		methods = append(methods, 0x02)
	}
	if _, err = connection.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fail(err)
	}
	response := make([]byte, 2)
	if _, err = io.ReadFull(connection, response); err != nil {
		return fail(fmt.Errorf("SOCKS greeting failed: %w", err))
	}
	if response[0] != 0x05 {
		return fail(fmt.Errorf("SOCKS greeting has invalid version"))
	}
	switch response[1] {
	case 0x00:
	case 0x02:
		if proxy.Username == "" && proxy.Password == "" {
			return fail(fmt.Errorf("SOCKS proxy requires credentials"))
		}
		auth := []byte{0x01, byte(len(proxy.Username))}
		auth = append(auth, proxy.Username...)
		auth = append(auth, byte(len(proxy.Password)))
		auth = append(auth, proxy.Password...)
		if _, err = connection.Write(auth); err != nil {
			return fail(err)
		}
		if _, err = io.ReadFull(connection, response); err != nil || response[1] != 0x00 {
			return fail(fmt.Errorf("SOCKS authentication failed"))
		}
	default:
		return fail(fmt.Errorf("SOCKS authentication method rejected"))
	}

	address := destination.To4()
	atyp := byte(0x01)
	if address == nil {
		address = destination.To16()
		atyp = 0x04
	}
	if address == nil {
		return fail(fmt.Errorf("invalid destination address"))
	}
	request := []byte{0x05, 0x01, 0x00, atyp}
	request = append(request, address...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	request = append(request, portBytes...)
	if _, err = connection.Write(request); err != nil {
		return fail(err)
	}
	reader := bufio.NewReader(connection)
	header := make([]byte, 4)
	if _, err = io.ReadFull(reader, header); err != nil {
		return fail(fmt.Errorf("SOCKS connect response: %w", err))
	}
	if header[0] != 0x05 || header[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS connect rejected: code=%d", header[1]))
	}
	boundLength := 0
	switch header[3] {
	case 0x01:
		boundLength = 4
	case 0x04:
		boundLength = 16
	case 0x03:
		length, readErr := reader.ReadByte()
		if readErr != nil {
			return fail(readErr)
		}
		boundLength = int(length)
	default:
		return fail(fmt.Errorf("SOCKS returned invalid address type"))
	}
	if _, err = io.CopyN(io.Discard, reader, int64(boundLength+2)); err != nil {
		return fail(err)
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, nil
}
