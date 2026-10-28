package datapath

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConnectionTraceBoundedAndExported(t *testing.T) {
	p := newMTProxyPool()
	defer p.cancel()
	for i := 0; i < 200; i++ {
		id := p.beginTrace("149.154.167.255")
		p.trace(id, "149.154.167.255", "tcp-dial-failed", "direct", 0)
	}
	var out struct {
		Connections []connectionTrace `json:"connections"`
	}
	if err := json.Unmarshal(p.statusJSON(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Connections) != 256 {
		t.Fatal(len(out.Connections))
	}
	last := out.Connections[255]
	if last.ID != 200 || last.Stage != "tcp-dial-failed" {
		t.Fatalf("bad event: %+v", last)
	}
	if _, err := time.Parse(time.RFC3339Nano, last.At); err != nil {
		t.Fatal(err)
	}
}
