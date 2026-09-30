package room

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestWorkspaceJournalOwnershipWaitRecoveryAndRelease(t *testing.T) {
	s := NewSharedCapacity()
	workspace := t.TempDir()
	var saved []byte
	if err := s.ConfigureWorkspaceJournal(nil, func(data []byte) error { saved = append([]byte(nil), data...); return nil }); err != nil {
		t.Fatal(err)
	}
	s.registerWriter("a", "one", "Room 78", "a", chat.Codex)
	if err := s.acquireWriter(t.Context(), workspace, "a", func() {}); err != nil {
		t.Fatal(err)
	}
	crash := append([]byte(nil), saved...)
	s.registerWriter("b", "two", "Room 82", "b", chat.Claude)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- s.acquireWriter(ctx, workspace, "b", func() { close(waiting) }) }()
	<-waiting
	a := s.Snapshot(workspace)
	if a.Owner.RoomName != "Room 78" || len(a.Waiting) != 1 || a.Waiting[0].RoomName != "Room 82" {
		t.Fatalf("snapshot: %+v", a)
	}
	a.Owner.RoomName = "tampered"
	if s.Snapshot(workspace).Owner.RoomName != "Room 78" {
		t.Fatal("mutable snapshot")
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(s.Snapshot(workspace).Waiting) != 0 || s.records["b"] != nil {
		t.Fatal("cancelled waiter leaked")
	}
	s.retainWriterTurn("a")
	s.releaseWriter(workspace, "a")
	if s.Snapshot(workspace).Owner == nil {
		t.Fatal("early release before native call stopped")
	}
	s.finishWriterTurn(workspace, "a")
	var journal map[string]chat.WorkspaceActivity
	if err := json.Unmarshal(saved, &journal); err != nil {
		t.Fatal(err)
	}
	if journal[workspace].Owner != nil || journal[workspace].LastWriter.ReleaseReason == "" {
		t.Fatal("release not journaled")
	}
	clean := NewSharedCapacity()
	if err := clean.ConfigureWorkspaceJournal(saved, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if a := clean.Snapshot(workspace); a.RecoveryRequired || a.LastWriter == nil || a.Owner != nil {
		t.Fatal("clean restart lost history or created a recovery hold", a)
	}
	restarted := NewSharedCapacity()
	if err := restarted.ConfigureWorkspaceJournal(crash, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !restarted.Snapshot(workspace).RecoveryRequired {
		t.Fatal("crashed writer silently cleared")
	}
	restarted.registerWriter("c", "three", "Room 83", "c", chat.Codex)
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	if err := restarted.acquireWriter(ctx2, workspace, "c", func() {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("recovery must block new writes", err)
	}
	o := &Orchestrator{sharedCapacity: restarted, sharedWorkspace: workspace}
	if err := o.ConfirmWorkspaceStopped(); err != nil {
		t.Fatal(err)
	}
	if restarted.Snapshot(workspace).RecoveryRequired {
		t.Fatal("confirmation did not clear hold")
	}
	restarted.registerWriter("c", "three", "Room 83", "c", chat.Codex)
	if err := restarted.acquireWriter(t.Context(), workspace, "c", func() {}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceJournalFailureCannotGrantWriter(t *testing.T) {
	s := NewSharedCapacity()
	s.ConfigureWorkspaceJournal(nil, func([]byte) error { return errors.New("disk full") })
	s.registerWriter("a", "one", "Room 78", "a", chat.Codex)
	if err := s.acquireWriter(t.Context(), "workspace", "a", func() {}); err == nil {
		t.Fatal("writer started without durable ownership")
	}
	if s.writers["workspace"] != "" || !s.Snapshot("workspace").RecoveryRequired {
		t.Fatal("failed journal not exposed")
	}
}

func TestWorkspaceFilesNormalizeAndKeepEvidenceSeparate(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := os.WriteFile(filepath.Join(root, "backlog.md"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := mergeFiles(nil, root, []string{"backlog.md", "./backlog.md", filepath.Join(root, "backlog.md"), "../outside", "bad\npath", "new/sub.md"})
	if strings.Join(paths, ",") != "backlog.md,new/sub.md" {
		t.Fatal(paths)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err == nil {
		if got := workspaceFile(root, "escape/new.md"); got != "" {
			t.Fatal("outside symlink allowed", got)
		}
	}
	s := NewSharedCapacity()
	s.registerWriter("one:w", "one", "Room 78", "w", chat.Codex)
	o := &Orchestrator{sharedCapacity: s, sharedWorkspace: root, room: chat.Room{ID: "one"}, writerWorkflow: "w"}
	o.recordWorkspaceFiles("w", []string{"backlog.md"}, []string{"new/sub.md"})
	r := s.records["one:w"]
	if len(r.IntendedFiles) != 1 || len(r.ObservedFiles) != 1 || !r.FilesPartial {
		t.Fatal(r)
	}
	s.acquireWriter(t.Context(), root, "one:w", func() {})
	s.releaseWriter(root, "one:w")
	guidance := o.workspaceWriterGuidance("w")
	if !strings.Contains(guidance, "Room 78") || !strings.Contains(guidance, "reread") {
		t.Fatal(guidance)
	}
}
