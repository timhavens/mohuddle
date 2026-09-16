package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWSLBuildKeepsLauncherAndRunningImages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX source-build workflow")
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	// Exercise the real recipes offline, without compiling the whole app again.
	// An inert compiler and kernel name select the WSL branch on Linux/macOS CI.
	root := filepath.Join(t.TempDir(), "source with spaces")
	fakeTools := filepath.Join(root, "tools")
	if err := os.MkdirAll(fakeTools, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Makefile", "scripts/mohuddle-launcher.sh"} {
		data, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	compiler := "#!/bin/sh\nset -eu\nwhile [ \"$#\" -gt 0 ]; do\ncase \"$1\" in -o) output=$2; shift;; esac\nshift\ndone\ncp \"$MOHUDDLE_BUILD_TEST_IMAGE\" \"$output\"\nchmod 0755 \"$output\"\n"
	for name, script := range map[string]string{"go": compiler, "uname": "#!/bin/sh\nprintf '%s\\n' test-microsoft-WSL2\n"} {
		if err := os.WriteFile(filepath.Join(fakeTools, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	payload := filepath.Join(root, "test-image")
	writeImage := func(version string) {
		t.Helper()
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' '" + version + "'; else printf '<%s>\\n' \"$@\"; fi\n"
		if err := os.WriteFile(payload, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runMake := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, makePath, append([]string{"VERSION=build-test"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+fakeTools+string(os.PathListSeparator)+os.Getenv("PATH"), "MOHUDDLE_BUILD_TEST_IMAGE="+payload)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make: %v\n%s", err, data)
		}
	}
	run := func(path string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), path, args...)
		cmd.Dir = t.TempDir() // invocation must not depend on the repository cwd
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("launch: %v\n%s", err, data)
		}
		return string(data)
	}
	writeImage("first")
	runMake("build")
	entry := filepath.Join(root, "bin", "mohuddle")
	before, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "bin", ".mohuddle-install-source"))
	if err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(root, strings.TrimSpace(string(manifest)))
	writeImage("second")
	runMake("build")
	after, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("rebuild replaced the stable launcher")
	}
	if got := run(entry, "--version"); got != "second\n" {
		t.Fatal(got)
	}
	if got := run(previous, "--version"); got != "first\n" {
		t.Fatal("previous running build changed:", got)
	}
	if got := run(entry, "two words", "", "a'b", "$literal"); got != "<two words>\n<>\n<a'b>\n<$literal>\n" {
		t.Fatal("launcher changed arguments:", got)
	}
	prefix := filepath.Join(t.TempDir(), "install with spaces")
	runMake("install", "PREFIX="+prefix)
	installed := filepath.Join(prefix, "bin", "mohuddle")
	expected, _ := os.ReadFile(payload)
	actual, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("install copied the repository launcher instead of the executable")
	}
	if err := os.RemoveAll(filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	if got := run(installed, "--version"); got != "second\n" {
		t.Fatal("installed executable depends on repository builds:", got)
	}
}
