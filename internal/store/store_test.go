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
	message := chat.Message{
		ID: "message", Sequence: 1, Author: chat.Claude, Kind: chat.MessageText, Text: "hello", CreatedAt: time.Now().UTC(),
		CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionAccepted, CorrectionSequence: 42}},
	}
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
	if loadedRoom.Workspace != workspace || len(messages) != 1 || messages[0].Text != "hello" || len(messages[0].CorrectionEvents) != 1 || messages[0].CorrectionEvents[0].CorrectionSequence != 42 {
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

func TestComposerHistoryAndAttachmentsRoundTripPrivately(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := value.SaveAttachment(room.ID, chat.Attachment{
		Kind: chat.AttachmentImage, Name: "clipboard.png", MIMEType: "image/png", Width: 10, Height: 20,
	}, []byte("png data"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []chat.ComposerHistoryEntry{{
		Text: "review this", Pastes: []string{"long pasted text"}, Attachments: []chat.Attachment{attachment}, CreatedAt: time.Now().UTC(),
	}}
	if err := value.SaveComposerHistory(room.ID, entries); err != nil {
		t.Fatal(err)
	}
	loaded, err := value.LoadComposerHistory(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Text != "review this" || loaded[0].Pastes[0] != "long pasted text" || loaded[0].Attachments[0].Path != attachment.Path {
		t.Fatalf("history=%+v", loaded)
	}
	if data, err := os.ReadFile(attachment.Path); err != nil || string(data) != "png data" {
		t.Fatalf("attachment data=%q err=%v", data, err)
	}
	assertMode(t, filepath.Join(value.roomDir(room.ID), composerFile), 0o600)
	assertMode(t, filepath.Dir(attachment.Path), 0o700)
	assertMode(t, attachment.Path, 0o600)
}

func TestComposerHistoryKeepsNewestTwoHundredEntries(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]chat.ComposerHistoryEntry, 205)
	for index := range entries {
		entries[index] = chat.ComposerHistoryEntry{Text: string(rune('a' + index%26)), CreatedAt: time.Now().UTC()}
	}
	if err := value.SaveComposerHistory(room.ID, entries); err != nil {
		t.Fatal(err)
	}
	loaded, err := value.LoadComposerHistory(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 200 || loaded[0].Text != entries[5].Text {
		t.Fatalf("history length=%d first=%q", len(loaded), loaded[0].Text)
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

func TestLegacyRoomWithoutMaxWavesMigratesToThree(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	room.MaxWaves = 0
	room.MaxTurns = 8
	if err := value.SaveRoom(room); err != nil {
		t.Fatal(err)
	}
	loaded, err := value.LoadRoom(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MaxWaves != 3 || loaded.MaxTurns != 8 {
		t.Fatalf("migrated room max_waves=%d max_turns=%d", loaded.MaxWaves, loaded.MaxTurns)
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
