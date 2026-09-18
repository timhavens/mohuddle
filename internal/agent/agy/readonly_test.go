package agy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Argument/protocol tests use a fake launcher. The separate boundary test
// below executes the real OS sandbox and checks file contents after writes.
func fakeReadOnlyWrapper(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name, _, err := readOnlyWrapper(runtime.GOOS)
	if err != nil {
		t.Skip(err)
	}
	script := "#!/bin/sh\n"
	if name == "bwrap" {
		script += "while [ \"$1\" != -- ]; do shift; done\nshift\n"
	} else {
		script += "shift 2\n"
	}
	script += "exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestReadOnlyWrapperFailsClosedWithoutSupport(t *testing.T) {
	if _, _, err := readOnlyWrapper("windows"); err == nil {
		t.Fatal("unsupported platform silently permits writes")
	}
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := readOnlyCommand(context.Background(), "/bin/sh", nil); err == nil {
		t.Fatal("missing sandbox silently permits writes")
	}
}

func TestReadOnlyBoundaryAllowsReadsAndBlocksWrites(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no supported OS write sandbox")
	}
	wrapper, _, _ := readOnlyWrapper(runtime.GOOS)
	if _, err := exec.LookPath(wrapper); err != nil {
		t.Skip("OS sandbox not installed")
	}
	probe, err := readOnlyCommand(context.Background(), "/bin/sh", []string{"-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("OS sandbox unavailable in this environment: %v: %s", err, output)
	}
	workspace, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(workspace, "fixture")
	external := filepath.Join(outside, "fixture")
	for _, path := range []string{inside, external} {
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(workspace, "outside-link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	script := `for path do
  test "$(cat "$path")" = original || exit 10
  if printf changed > "$path"; then exit 11; fi
  if rm "$path"; then exit 12; fi
done
printf 'reads succeeded; writes denied'
`
	cmd, err := readOnlyCommand(context.Background(), "/bin/sh", []string{"-c", script, "probe", inside, external, link})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "reads succeeded; writes denied") {
		t.Fatalf("boundary failed: %v: %s", err, output)
	}
	for _, path := range []string{inside, external, link} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Fatalf("fixture changed: %s", path)
		}
	}
}
