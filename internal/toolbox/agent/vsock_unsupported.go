//go:build !linux

package agent

import (
	"errors"
	"net"
)

func ListenVsock(port uint32) (net.Listener, error) {
	_ = port
	return nil, errors.New("vsock listener is only supported on linux")
}
