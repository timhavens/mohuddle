package agy

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// AGY's --mode plan is a prompt prefix, and --sandbox constrains terminal
// commands only. Neither blocks its direct file-write tools. Wrap the entire
// provider process so reads keep their scope while host files stay read-only.
// No writable host directory is exempted, including AGY's own settings/cache.
func readOnlyCommand(ctx context.Context, binary string, args []string) (*exec.Cmd, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find AGY: %w", err)
	}
	wrapper, prefix, err := readOnlyWrapper(runtime.GOOS)
	if err != nil {
		return nil, err
	}
	launcher, err := exec.LookPath(wrapper)
	if err != nil {
		return nil, fmt.Errorf("AGY read-only inspection requires %s; refusing to run without enforced write protection: %w", wrapper, err)
	}
	return exec.CommandContext(ctx, launcher, append(append(prefix, path), args...)...), nil
}

func readOnlyWrapper(platform string) (string, []string, error) {
	switch platform {
	case "linux":
		return "bwrap", []string{
			"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
			// /dev/shm belongs to the private /dev mount, not the host.
			"--setenv", "TMPDIR", "/dev/shm",
			"--unshare-pid", "--unshare-ipc", "--new-session", "--die-with-parent",
			"--cap-drop", "ALL", "--",
		}, nil
	case "darwin":
		return "sandbox-exec", []string{"-p", `(version 1) (allow default) (deny file-write*) (allow file-write-data (literal "/dev/null"))`}, nil
	default:
		return "", nil, fmt.Errorf("AGY read-only inspection is unavailable on %s: no supported filesystem write sandbox; use Codex, Claude, or Copilot for this task", platform)
	}
}
