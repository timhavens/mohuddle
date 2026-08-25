// Package testutil provides portable helpers shared by package tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// CanonicalTempDir returns a test-owned temporary directory after resolving
// platform aliases such as macOS /var -> /private/var and Windows short paths.
func CanonicalTempDir(t testing.TB) string {
	t.Helper()
	directory := t.TempDir()
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("canonicalize temporary directory: %v", err)
	}
	absolute, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return filepath.Clean(absolute)
}

// ShortTempDir returns a private test-owned directory suitable for Unix
// domain socket paths, whose platform length limit can be much lower than the
// path produced by testing.T.TempDir.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return CanonicalTempDir(t)
	}
	directory, err := os.MkdirTemp("/tmp", "mh-")
	if err != nil {
		t.Fatalf("create short temporary directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		t.Fatalf("secure short temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
