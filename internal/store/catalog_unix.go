//go:build !windows

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockCatalog(path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = f.Close() }, nil
}
