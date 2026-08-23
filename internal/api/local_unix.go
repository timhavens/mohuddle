//go:build !windows

package api

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

type localListener struct {
	net.Listener
	path string
}

func LocalTransportSupported() bool { return true }

func DefaultSocketPath(stateRoot, roomID string) string {
	return filepath.Join(stateRoot, "api-"+roomID+".sock")
}

func listenLocal(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("local API socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("local API socket parent must be a private directory (0700): %s", filepath.Dir(path))
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("local API path exists and is not a socket: %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			connection.Close()
			return nil, fmt.Errorf("local API socket is already active: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	return &localListener{Listener: listener, path: path}, nil
}

func (l *localListener) Close() error {
	err := l.Listener.Close()
	removeErr := os.Remove(l.path)
	if err != nil {
		return err
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}
