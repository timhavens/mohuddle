//go:build windows

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
)

func lockTunnel(string) (*os.File, error) {
	return nil, fmt.Errorf("ChatGPT's private room transport requires Linux/WSL or macOS")
}
func prepareProcess(*exec.Cmd)                        {}
func stopProcess(p *os.Process, done <-chan struct{}) { _ = p.Kill(); <-done }
func foreignProcess(string, string, string) bool      { return false }
