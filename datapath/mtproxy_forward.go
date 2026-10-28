//go:build android || linux

package datapath

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

func handleTelegramMTProxy(client net.Conn, dialer *net.Dialer, destination net.IP, first []byte, policy *lightPolicy) bool {
	traceID := policy.mtproxies.beginTrace(destination.String())
	dc := telegramProxyDC(destination)
	if dc == 0 {
		kind := "unknown"
		if _, _, tls := findTLSServerName(first); tls {
			kind = "tls"
		}
		// Never consume client bytes just to classify an unmapped endpoint:
		// the fallback must receive the original, intact stream.
		policy.mtproxies.transportEvent("unmapped/"+destination.String()+"/"+kind, 1)
		// Public service endpoint metadata only, bounded; no account or payload.
		policy.mtproxies.mu.Lock()
		if policy.mtproxies.unmapped == nil {
			policy.mtproxies.unmapped = map[string]int{}
		}
		if len(policy.mtproxies.unmapped) < 16 || policy.mtproxies.unmapped[destination.String()] > 0 {
			policy.mtproxies.unmapped[destination.String()]++
		}
		policy.mtproxies.mu.Unlock()
		policy.mtproxies.trace(traceID, destination.String(), "unmapped-fallback", "", 0)
		return false
	}
	local, tag, err := unwrapLocalMTProto(client, first)
	if err != nil {
		policy.mtproxies.mu.Lock()
		policy.mtproxies.unrecognized++
		policy.mtproxies.mu.Unlock()
		policy.mtproxies.trace(traceID, destination.String(), "unsupported-header-fallback", "", 0)
		return false
	}
	event := func(phase string, count int64) {
		policy.mtproxies.transportEvent(fmt.Sprintf("dc%d/%s", dc, phase), count)
	}
	event("attempt", 1)
	dialStarted := time.Now()
	remote, id, err := policy.mtproxies.dial(dialer, policy.doh, dc)
	if err != nil {
		event("dial-failed", 1)
		policy.mtproxies.trace(traceID, destination.String(), "relay-dial-failed", fmt.Sprintf("dc%d", dc), 0)
		policy.recordConnect(serviceTelegram, false, "")
		policy.mtproxies.waitAfterDialFailure(dialStarted)
		return true
	}
	defer remote.Close()
	event("connected", 1)
	policy.mtproxies.trace(traceID, destination.String(), "relay-connected", fmt.Sprintf("dc%d/%s", dc, id), 0)
	policy.recordConnect(serviceTelegram, true, "mtproxy-"+id)
	type outcome struct {
		server          bool
		bytes           int64
		err             error
		upstreamFailure bool
	}
	done := make(chan outcome, 2)
	go func() {
		for {
			frame, err := readMTFrame(local, tag, false)
			if err != nil {
				event("client-read-ended", 1)
				done <- outcome{err: err}
				return
			}
			remote.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if err = writeMTFrame(remote, 0xdd, frame); err != nil {
				event("upstream-write-failed", 1)
				done <- outcome{err: err, upstreamFailure: true}
				return
			}
		}
	}()
	go func() {
		var received int64
		remote.SetReadDeadline(time.Now().Add(12 * time.Second))
		for {
			frame, err := readMTFrame(remote, 0xdd, true)
			if err != nil {
				if received == 0 {
					event("server-ended-without-data", 1)
				} else {
					event("server-read-ended", 1)
				}
				done <- outcome{server: true, bytes: received, err: err, upstreamFailure: !mtProxyNormalStreamEnd(received, err)}
				return
			}
			local.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if err = writeMTFrame(local, tag, frame); err != nil {
				event("client-write-failed", 1)
				// The application's closed socket says nothing about proxy health.
				done <- outcome{server: true, bytes: received, err: err}
				return
			}
			received += int64(len(frame.body))
			event("received-bytes", int64(len(frame.body)))
			policy.mtproxies.mu.Lock()
			policy.mtproxies.received += int64(len(frame.body))
			policy.mtproxies.lastStage = "relaying"
			policy.mtproxies.mu.Unlock()
			remote.SetReadDeadline(time.Now().Add(120 * time.Second))
		}
	}()
	a := <-done
	// Only the first completion caused shutdown. The other pump's error after
	// Close is ours, not evidence that the remote endpoint stopped working.
	remoteFailed := a.upstreamFailure
	endStage := "client-ended"
	if a.server {
		endStage = "server-ended"
	}
	if remoteFailed {
		endStage = "upstream-failed"
	}
	local.Close()
	remote.Close()
	b := <-done
	if !a.server {
		a = b
	}
	policy.mtproxies.trace(traceID, destination.String(), endStage, fmt.Sprintf("dc%d/%s", dc, id), a.bytes)
	if success, report := mtProxyStreamHealth(a.bytes, remoteFailed); report {
		policy.mtproxies.report(id, dc, success)
		policy.recordTCP(serviceTelegram, strategyDirect, success)
	}
	if remoteFailed {
		policy.mtproxies.discoverForDC(dialer, policy.doh, dc)
	}
	return true
}

func mtProxyStreamHealth(received int64, upstreamFailed bool) (success, report bool) {
	if upstreamFailed {
		return false, true
	}
	if received > 0 {
		return true, true
	}
	// A locally cancelled request with no response is neither success nor a
	// failed proxy. In particular, do not invalidate a previously verified IP.
	return false, false
}

func mtProxyNormalStreamEnd(received int64, err error) bool {
	if received <= 0 {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var timeout net.Error
	// Our 120-second read-idle deadline after successful data is not a failed
	// dial/proxy. The first-response deadline still fails when received == 0.
	return errors.As(err, &timeout) && timeout.Timeout()
}
