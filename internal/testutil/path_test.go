package testutil

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCanonicalTempDirIsResolved(t *testing.T) {
	directory := CanonicalTempDir(t)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != directory {
		t.Fatalf("temporary directory = %q, resolved = %q", directory, resolved)
	}
}

func TestShortTempDirKeepsUnixSocketPathsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket path limits do not apply")
	}
	directory := ShortTempDir(t)
	if !strings.HasPrefix(directory, "/tmp/mh-") {
		t.Fatalf("short temporary directory = %q", directory)
	}
	if len(filepath.Join(directory, "api.sock")) >= 64 {
		t.Fatalf("socket test path is unexpectedly long: %q", directory)
	}
}
