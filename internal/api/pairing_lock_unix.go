//go:build !windows

package api

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquirePairingFileLock(statePath string) (func(), error) {
	directory := filepath.Dir(statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	path := statePath + ".lock"
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("federation pairing lock path must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
