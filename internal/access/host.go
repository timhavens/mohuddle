package access

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// HostRoots describes the host filesystem, not additional persisted grants.
// OS account permissions still apply. Include mounted Windows volumes and the
// workspace's UNC volume; Unix mounts are all reachable beneath /.
func HostRoots(workspace string) []string {
	if runtime.GOOS != "windows" {
		return []string{"/"}
	}
	var roots []string
	for drive := 'A'; drive <= 'Z'; drive++ {
		root := string(drive) + ":\\"
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			roots = append(roots, root)
		}
	}
	if volume := filepath.VolumeName(workspace); volume != "" {
		roots = append(roots, volume+string(filepath.Separator))
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}
