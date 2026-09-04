//go:build !windows

package store

// transientSharingViolation is always false outside Windows: renaming over a
// path never makes the destination temporarily unopenable on POSIX systems.
func transientSharingViolation(error) bool { return false }
