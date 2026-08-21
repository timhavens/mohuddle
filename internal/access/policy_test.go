package access

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestCanonicalDirectoryAndContains(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalDirectory(filepath.Join(child, "..", "child"))
	if err != nil {
		t.Fatal(err)
	}
	if canonical != child {
		t.Fatalf("canonical=%q want %q", canonical, child)
	}
	if !Contains(root, child) || Contains(child, root) {
		t.Fatal("containment check failed")
	}
}

func TestCanonicalDirectoryResolvesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privileges")
	}
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalDirectory(link)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != target {
		t.Fatalf("canonical=%q want %q", canonical, target)
	}
	if Contains(root, canonical) {
		t.Fatal("symlink target incorrectly treated as inside root")
	}
}

func TestEffectiveRootsByParticipantAndMode(t *testing.T) {
	now := time.Now()
	room := chat.NewRoom("room", "/workspace", 4, now)
	room.Grants = append(room.Grants,
		chat.AccessGrant{Path: "/shared", Mode: chat.AccessRead, Participant: chat.System},
		chat.AccessGrant{Path: "/codex-write", Mode: chat.AccessReadWrite, Participant: chat.Codex},
		chat.AccessGrant{Path: "/claude-write", Mode: chat.AccessReadWrite, Participant: chat.Claude},
	)
	reads := EffectiveRoots(room, chat.Codex, chat.AccessRead)
	writes := EffectiveRoots(room, chat.Codex, chat.AccessReadWrite)
	if len(reads) != 3 || len(writes) != 2 {
		t.Fatalf("unexpected roots: read=%v write=%v", reads, writes)
	}
}
