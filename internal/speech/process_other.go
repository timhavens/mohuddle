//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package speech

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func runPlayback(ctx context.Context, binary string, arguments ...string) error {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("edge-playback failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func prepareProcess(*exec.Cmd) {}

func setProcessNice(*os.Process, int) error { return nil }

func stopProcess(process *os.Process, done <-chan error) {
	if process == nil {
		return
	}
	_ = process.Kill()
	<-done
}

func interruptProcess(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}

func killProcess(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}
