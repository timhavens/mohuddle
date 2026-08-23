//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package speech

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func runPlayback(ctx context.Context, binary string, arguments ...string) error {
	command := exec.Command(binary, arguments...)
	prepareProcess(command)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start edge-playback: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("edge-playback failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	case <-ctx.Done():
		stopProcess(command.Process, done)
		return ctx.Err()
	}
}

func prepareProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func setProcessNice(process *os.Process, value int) error {
	if process == nil || value == 0 {
		return nil
	}
	return syscall.Setpriority(syscall.PRIO_PROCESS, process.Pid, value)
}

func stopProcess(process *os.Process, done <-chan error) {
	if process == nil {
		return
	}
	interruptProcess(process)
	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		killProcess(process)
		<-done
	}
}

func interruptProcess(process *os.Process) {
	if process != nil {
		_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
	}
}

func killProcess(process *os.Process) {
	if process != nil {
		_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	}
}
