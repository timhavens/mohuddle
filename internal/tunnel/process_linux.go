//go:build linux

package tunnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func parentDeathSignal(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM }

func foreignProcess(profilePath, id, name string) bool {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		if len(args) < 2 || !strings.HasPrefix(filepath.Base(args[0]), "tunnel-client") || args[1] != "run" {
			continue
		}
		for i, arg := range args[2:] {
			key, value, eq := strings.Cut(arg, "=")
			if !eq && i+3 < len(args) {
				value = args[i+3]
			}
			switch key {
			case "--profile":
				if value == name {
					return true
				}
			case "--profile-file", "--config":
				if value == profilePath {
					return true
				}
			case "--control-plane.tunnel-id":
				if value == id {
					return true
				}
			}
		}
	}
	return false
}
