package store

import (
	"errors"
	"syscall"
)

// Windows system error numbers reported while another handle to a path is
// still being replaced. They are spelled out here so the check does not depend
// on which names the standard library happens to export.
const (
	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// transientSharingViolation reports whether Windows refused the open because
// another handle to the path is still being replaced.
func transientSharingViolation(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errorSharingViolation || errno == errorLockViolation || errno == errorAccessDenied
}
