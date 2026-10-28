//go:build android

package datapath

/*
#cgo LDFLAGS: -landroid
#include <android/multinetwork.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
)

type networkSelector struct{ handle atomic.Uint64 }

func (n *networkSelector) set(handle uint64) { n.handle.Store(handle) }

func (n *networkSelector) verify() error {
	handle := n.handle.Load()
	if handle == 0 {
		return errors.New("fail-closed: no VPN network selected")
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return bindSocket(handle, uintptr(fd))
}

func (n *networkSelector) control(_, _ string, raw syscall.RawConn) error {
	handle := n.handle.Load()
	if handle == 0 {
		return errors.New("fail-closed: no VPN network selected")
	}
	var bindFailure error
	if err := raw.Control(func(fd uintptr) {
		bindFailure = bindSocket(handle, fd)
	}); err != nil {
		return err
	}
	return bindFailure
}

func bindSocket(handle uint64, fd uintptr) error {
	if result, errno := C.android_setsocknetwork(C.net_handle_t(handle), C.int(fd)); result != 0 {
		return fmt.Errorf("android_setsocknetwork(%d): %w", handle, errno)
	}
	return nil
}
