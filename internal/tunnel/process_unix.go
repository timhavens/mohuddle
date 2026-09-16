//go:build !windows

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func lockTunnel(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open private tunnel ownership lock")
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("this tunnel is already managed by another MoHuddle room; leave it there first")
	}
	return f, nil
}

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parentDeathSignal(cmd)
}

func stopProcess(p *os.Process, done <-chan struct{}) {
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	// The stdio bridge may outlive its parent. Clean up only the group we own.
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	<-done
}
