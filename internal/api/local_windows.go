//go:build windows

package api

import (
	"fmt"
	"net"
)

func LocalTransportSupported() bool { return false }

func DefaultSocketPath(_, roomID string) string { return `\\.\pipe\mohuddle-` + roomID }

func listenLocal(string) (net.Listener, error) {
	return nil, fmt.Errorf("the local API named-pipe transport is not implemented on Windows yet")
}
