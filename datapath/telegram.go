//go:build android || linux

package datapath

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"
)

// ProbeTelegramNetwork performs a direct, service-specific baseline on the
// selected Android Network before LIGHT is enabled. It races Telegram's
// documented TCP listeners over both address families and accepts only a
// nonce-matching MTProto resPQ.
func ProbeTelegramNetwork(networkHandle int64) bool {
	if networkHandle <= 0 {
		return false
	}
	selector := &networkSelector{}
	selector.set(uint64(networkHandle))
	dialer := &net.Dialer{Timeout: 3 * time.Second, Control: selector.control}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	targets := telegramProbeTargets()
	ports := []uint16{443, 80, 5222}
	results := make(chan bool, len(targets)*len(ports))
	for _, target := range targets {
		for _, port := range ports {
			target, port := target, port
			go func() {
				address := net.JoinHostPort(target.String(), strconv.Itoa(int(port)))
				connection, err := dialer.DialContext(ctx, "tcp", address)
				if err != nil {
					results <- false
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(4 * time.Second))
				payload, nonce, err := telegramReqPQ()
				if err == nil {
					err = writeAll(connection, payload)
				}
				results <- err == nil && readTelegramResPQ(connection, nonce)
			}()
		}
	}
	for range cap(results) {
		select {
		case success := <-results:
			if success {
				cancel()
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// ProbeTelegram performs the unauthenticated first step of Telegram's public
// MTProto key exchange. A green result therefore requires an actual Telegram
// DC response, not the unrelated core.telegram.org website.
func (e *Engine) ProbeTelegram() bool {
	if e == nil || e.policy == nil || e.policy.doh == nil {
		return false
	}
	targets := telegramProbeTargets()
	index := int(e.policy.telegramProbe.Add(1)-1) % len(targets)
	destination := targets[index]
	if e.policy.enabled.Load() && e.policy.mtproxies.enabled() {
		dc := telegramDCNumber(destination)
		c, id, err := e.policy.mtproxies.dial(e.policy.doh.dialer, e.policy.doh, dc)
		if err != nil {
			e.policy.recordConnect(serviceTelegram, false, "")
			return false
		}
		err = verifyMTProxyConnection(c)
		c.Close()
		ok := err == nil
		e.policy.mtproxies.report(id, dc, ok)
		e.policy.recordConnect(serviceTelegram, true, "mtproxy-"+id)
		e.policy.recordTCP(serviceTelegram, strategyDirect, ok)
		return ok
	}
	connection, route, err := dialTCPForService(
		e.policy.doh.dialer,
		e.policy,
		serviceTelegram,
		destination,
		443,
		true,
		nil,
		e.policy.nat64Address(destination),
	)
	if err != nil {
		e.policy.recordConnect(serviceTelegram, false, "")
		return false
	}
	defer connection.Close()
	tuneTCP(connection)
	_ = connection.SetDeadline(time.Now().Add(4 * time.Second))
	payload, nonce, err := telegramReqPQ()
	if err != nil {
		return false
	}
	strategy := e.policy.choose(serviceTelegram, payload)
	success := false
	if strings.HasPrefix(route, "telegram-wss-dc") {
		success = probeTelegramObfuscated(connection, telegramDCNumber(destination), payload, nonce)
	} else {
		if err := applyStrategy(connection, payload, strategy); err != nil {
			e.policy.proxies.report(route, false)
			e.policy.recordTCP(serviceTelegram, strategy, false)
			return false
		}
		success = readTelegramResPQ(connection, nonce)
		// A carrier may complete the TCP handshake and then discard MTProto.
		// In that case try Telegram's own WSS endpoint before declaring the
		// strategy exhausted, because connect-only evidence is insufficient.
		if !success {
			_ = connection.Close()
			if wss, wssRoute, wssErr := dialTelegramWebSocket(
				e.policy.doh.dialer,
				e.policy,
				destination,
			); wssErr == nil {
				_ = wss.SetDeadline(time.Now().Add(4 * time.Second))
				success = probeTelegramObfuscated(
					wss,
					telegramDCNumber(destination),
					payload,
					nonce,
				)
				_ = wss.Close()
				if success {
					route = wssRoute
				}
			}
		}
	}
	if success && strings.HasPrefix(route, "telegram-wss-dc") {
		e.policy.telegramWSS.Store(true)
	}
	e.policy.proxies.report(route, success)
	e.policy.recordConnect(serviceTelegram, true, route)
	e.policy.recordTCP(serviceTelegram, strategy, success)
	return success
}
