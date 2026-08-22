//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package speech

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunPlaybackCancellationKillsChildProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	binary := filepath.Join(dir, "edge-playback")
	writeExecutable(t, binary, `#!/bin/sh
sleep 30 &
child=$!
printf '%s' "$child" > "$MOHUDDLE_CHILD_PID"
wait "$child"
`)
	t.Setenv("MOHUDDLE_CHILD_PID", pidPath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runPlayback(ctx, binary, "--voice", "voice", "--text", "hello") }()

	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("playback child process did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d survived playback cancellation", childPID)
}
