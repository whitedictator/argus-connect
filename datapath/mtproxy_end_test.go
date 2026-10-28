//go:build android || linux

package datapath

import (
	"io"
	"os"
	"syscall"
	"testing"
)

func TestMTProxyStreamEndDoesNotPoisonHealthyEndpoint(t *testing.T) {
	if !mtProxyNormalStreamEnd(4096, io.EOF) {
		t.Fatal("normal server close blacklisted a working endpoint")
	}
	if !mtProxyNormalStreamEnd(4096, os.ErrDeadlineExceeded) {
		t.Fatal("local idle deadline blacklisted a working endpoint")
	}
	if mtProxyNormalStreamEnd(0, io.EOF) {
		t.Fatal("empty response treated as success")
	}
	if mtProxyNormalStreamEnd(4096, syscall.ECONNRESET) {
		t.Fatal("connection reset incorrectly treated as normal close")
	}
}

func TestMTProxyLocalCancellationIsNotUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		bytes                   int64
		failed, success, report bool
	}{
		{"cancel-before-response", 0, false, false, false},
		{"cancel-after-response", 4096, false, true, true},
		{"remote-no-response", 0, true, false, true},
		{"remote-reset-after-data", 4096, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			success, report := mtProxyStreamHealth(tc.bytes, tc.failed)
			if success != tc.success || report != tc.report {
				t.Fatalf("got success=%v report=%v", success, report)
			}
		})
	}
}
