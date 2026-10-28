package datapath

import "time"

// Bounded metadata only. No payloads, account IDs, secrets or raw errors.
type connectionTrace struct {
	ID          uint64 `json:"id"`
	At          string `json:"at"`
	Destination string `json:"destination"`
	Stage       string `json:"stage"`
	Route       string `json:"route,omitempty"`
	Bytes       int64  `json:"bytes"`
}

func (p *mtProxyPool) beginTrace(destination string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.traceSequence++
	p.appendTraceLocked(connectionTrace{ID: p.traceSequence, At: time.Now().UTC().Format(time.RFC3339Nano), Destination: destination, Stage: "start"})
	return p.traceSequence
}

func (p *mtProxyPool) trace(id uint64, destination, stage, route string, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendTraceLocked(connectionTrace{ID: id, At: time.Now().UTC().Format(time.RFC3339Nano), Destination: destination, Stage: stage, Route: route, Bytes: bytes})
}

func (p *mtProxyPool) appendTraceLocked(event connectionTrace) {
	if len(p.connectionTraces) == 256 {
		copy(p.connectionTraces, p.connectionTraces[1:])
		p.connectionTraces = p.connectionTraces[:255]
	}
	p.connectionTraces = append(p.connectionTraces, event)
}
