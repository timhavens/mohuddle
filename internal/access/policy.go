package access

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

func CanonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect path %q: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

func Contains(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func EffectiveRoots(room chat.Room, participant chat.Participant, mode chat.AccessMode) []string {
	var roots []string
	for _, grant := range room.Grants {
		if grant.Participant != chat.System && grant.Participant != participant {
			continue
		}
		if mode == chat.AccessReadWrite && grant.Mode != chat.AccessReadWrite {
			continue
		}
		roots = append(roots, grant.Path)
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

func Allowed(room chat.Room, participant chat.Participant, candidate string, mode chat.AccessMode) bool {
	canonical, err := CanonicalDirectory(candidate)
	if err != nil {
		return false
	}
	for _, root := range EffectiveRoots(room, participant, mode) {
		if Contains(root, canonical) {
			return true
		}
	}
	return false
}
