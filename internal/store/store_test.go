package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestDefaultStateDirUsesMoHuddleName(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	path, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(stateHome, "mohuddle")
	if path != want {
		t.Fatalf("state directory=%q want %q", path, want)
	}
}

func TestRoomAndTranscriptRoundTrip(t *testing.T) {
	state := t.TempDir()
	value, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	room, err := value.Create(workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	message := chat.Message{ID: "message", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "hello", CreatedAt: time.Now().UTC()}
	if err := value.AppendMessage(room.ID, message); err != nil {
		t.Fatal(err)
	}
	loadedRoom, err := value.LoadRoom(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := value.LoadMessages(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedRoom.Workspace != workspace || len(messages) != 1 || messages[0].Text != "hello" {
		t.Fatalf("unexpected round trip: room=%+v messages=%+v", loadedRoom, messages)
	}
	assertMode(t, state, 0o700)
	assertMode(t, filepath.Join(state, room.ID), 0o700)
	assertMode(t, filepath.Join(state, room.ID, roomFile), 0o600)
	assertMode(t, filepath.Join(state, room.ID, transcriptFile), 0o600)
}

func TestLoadMessagesIgnoresTruncatedFinalRecord(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	first := chat.Message{ID: "one", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "complete", CreatedAt: time.Now().UTC()}
	if err := value.AppendMessage(room.ID, first); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(value.roomDir(room.ID), transcriptFile)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"id":"partial"`)
	_ = file.Close()
	messages, err := value.LoadMessages(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Text != "complete" {
		t.Fatalf("unexpected recovered messages: %+v", messages)
	}
}

func TestListRoomsNewestFirst(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	one, _ := value.Create(t.TempDir(), 4)
	time.Sleep(2 * time.Millisecond)
	two, _ := value.Create(t.TempDir(), 4)
	rooms, err := value.ListRooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 || rooms[0].ID != two.ID || rooms[1].ID != one.ID {
		t.Fatalf("unexpected order: %+v", rooms)
	}
}

func assertMode(t *testing.T, path string, wanted os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != wanted {
		t.Fatalf("%s mode=%o want %o", path, info.Mode().Perm(), wanted)
	}
}
