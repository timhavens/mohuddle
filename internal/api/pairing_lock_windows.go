//go:build windows

package api

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func acquirePairingFileLock(statePath string) (func(), error) {
	directory := filepath.Dir(statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
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
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
	}, nil
}
