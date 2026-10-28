//go:build !android

package datapath

import (
	"errors"
	"syscall"
)

type networkSelector struct{}

func (*networkSelector) set(uint64) {}

func (*networkSelector) verify() error {
	return errors.New("Android network binding unavailable on host")
}

func (*networkSelector) control(_, _ string, _ syscall.RawConn) error {
	return errors.New("Android network binding unavailable on host")
}
