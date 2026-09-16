//go:build !linux && !windows

package tunnel

import "os/exec"

func parentDeathSignal(*exec.Cmd)                {}
func foreignProcess(string, string, string) bool { return false }
