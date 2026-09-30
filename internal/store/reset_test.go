package store

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestResetContextArchivesAndPreservesRoomSettings(t *testing.T) {
	s, _ := New(t.TempDir())
	r, _ := s.Create(t.TempDir(), 5)
	names, _ := s.RoomNames()
	r.RoomPrompt = "custom guidance"
	r.Sessions[chat.Codex] = chat.AgentSession{ID: "old-session"}
	r.Coordination = &chat.CoordinationRun{}
	r.FollowUps = &chat.FollowUps{Enabled: true, Attempts: 17, Revision: 4, StartedAt: time.Now().UTC(), NotifiedThrough: 99}
	if err := s.SaveRoom(r); err != nil {
		t.Fatal(err)
	}
	original := []byte("old transcript\n")
	os.WriteFile(filepath.Join(s.roomDir(r.ID), transcriptFile), original, 0600)
	os.MkdirAll(filepath.Join(s.roomDir(r.ID), attachmentsFolder), 0700)
	os.WriteFile(filepath.Join(s.roomDir(r.ID), attachmentsFolder, "test.png"), []byte("image"), 0600)
	workspaceFile := filepath.Join(r.Workspace, "keep.txt")
	os.WriteFile(workspaceFile, []byte("keep"), 0600)
	lock, err := s.AcquireRoomLock(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	archive, err := lock.ResetContext()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadRoom(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != r.ID || loaded.Workspace != r.Workspace || loaded.RoomPrompt != r.RoomPrompt || loaded.MaxWaves != 5 {
		t.Fatalf("settings changed: %+v", loaded)
	}
	if loaded.Sessions[chat.Codex].ID != "" || loaded.Coordination != nil {
		t.Fatal("old context survived")
	}
	if loaded.FollowUps.Attempts != 17 || !loaded.FollowUps.StartedAt.Equal(r.FollowUps.StartedAt) || loaded.FollowUps.NotifiedThrough != 0 || loaded.FollowUps.Revision != 5 {
		t.Fatal("allowance/context incorrect")
	}
	afterNames, _ := s.RoomNames()
	if afterNames[r.ID] != names[r.ID] {
		t.Fatal("room renamed")
	}
	messages, err := s.LoadMessages(r.ID)
	if err != nil || len(messages) != 0 {
		t.Fatal(messages, err)
	}
	if b, err := os.ReadFile(workspaceFile); err != nil || string(b) != "keep" {
		t.Fatal("workspace modified")
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == transcriptFile {
			b, _ := io.ReadAll(tr)
			found = string(b) == string(original)
		}
	}
	if !found {
		t.Fatal("original transcript not archived")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ResetContext(); err == nil {
		t.Fatal("released lock reset allowed")
	}
}

func TestResetRecoveryFinishesBeforeLoadingContext(t *testing.T) {
	s, _ := New(t.TempDir())
	r, _ := s.Create(t.TempDir(), 3)
	os.WriteFile(filepath.Join(s.roomDir(r.ID), transcriptFile), []byte("old data"), 0600)
	data, _ := json.Marshal(freshContext(r))
	if err := s.writeRoomFile(r.ID, resetPendingFile, data); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRoom(r.ID); err != nil {
		t.Fatal(err)
	}
	if messages, err := s.LoadMessages(r.ID); err != nil || len(messages) != 0 {
		t.Fatal(messages, err)
	}
	if _, err := os.Stat(filepath.Join(s.roomDir(r.ID), resetPendingFile)); !os.IsNotExist(err) {
		t.Fatal("intent not cleared", err)
	}
}

func TestResetArchiveFailureLeavesContextUntouched(t *testing.T) {
	s, _ := New(t.TempDir())
	r, _ := s.Create(t.TempDir(), 3)
	original, _ := os.ReadFile(filepath.Join(s.roomDir(r.ID), roomFile))
	if err := os.WriteFile(filepath.Join(s.root, "archives"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := s.AcquireRoomLock(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.ResetContext(); err == nil {
		t.Fatal("expected archive failure")
	}
	after, _ := os.ReadFile(filepath.Join(s.roomDir(r.ID), roomFile))
	if string(original) != string(after) {
		t.Fatal("room changed without archive")
	}
	if _, err := os.Stat(filepath.Join(s.roomDir(r.ID), resetPendingFile)); !os.IsNotExist(err) {
		t.Fatal("reset began without archive")
	}
}
