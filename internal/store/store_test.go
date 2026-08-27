package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		ID: "message", Sequence: 1, TurnID: "turn-1", Author: chat.User, Kind: chat.MessageText, WorkflowMode: chat.WorkflowPlan, DelegationPolicy: chat.DelegationAuto, Text: "hello", CreatedAt: time.Now().UTC(),
		CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionAccepted, CorrectionSequence: 42}},
		Route:            &chat.RouteMetadata{MessageID: "external", OriginInstanceID: "peer", OriginClientID: "peer/client", Hops: []string{"peer", "host"}},
	}
	planContent := "# Stored plan\n\n- Preserve exactly"
	plan := chat.ProposedPlan{
		ID: "plan", SourceMessageID: "source", SourceSequence: 2, Author: chat.Codex,
		Content: planContent, SHA256: chat.ProposedPlanHash(planContent), CreatedAt: time.Now().UTC(),
	}
	message.AcceptedPlan = &plan
	room.WorkflowMode = chat.WorkflowPlan
	room.DelegationPolicy = chat.DelegationAsk
	room.StreamMode = chat.StreamHistory
	room.TurnHistory = []chat.TurnRecord{{ID: "turn-1", Participant: chat.Codex, State: chat.TurnRecordFinal, Drafts: []string{"draft"}, Tools: []string{"tool"}, FinalSequence: 1, StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC()}}
	room.PendingPlan = &plan
	room.PendingDelegation = &chat.PendingDelegation{ID: "split", WorkflowVersion: 2, SourceSequence: 1, Requester: chat.Codex, Tasks: []chat.DelegationTask{{Participant: chat.Claude, Task: "inspect"}}, CreatedAt: time.Now().UTC()}
	room.Workflows["workflow-1"] = chat.WorkflowRecord{
		ID: "workflow-1", Generation: 1, SourceSequences: []uint64{1}, Target: chat.Codex, Lead: chat.Codex,
		Mode: chat.WorkflowPlan, DelegationPolicy: chat.DelegationAuto, Resource: chat.WorkflowReadOnly,
		State: chat.WorkflowWaiting, WaitReason: "human decision", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PendingPlan: &plan,
	}
	room.InputResolutions[1] = chat.InputResolution{SourceSequence: 1, WorkflowID: "workflow-1", Intent: chat.InputWork, ResolvedAt: time.Now().UTC()}
	if err := value.SaveRoom(room); err != nil {
		t.Fatal(err)
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
	if loadedRoom.SchemaVersion != 1 || loadedRoom.Workspace != workspace || loadedRoom.WorkflowMode != chat.WorkflowPlan || loadedRoom.DelegationPolicy != chat.DelegationAsk || loadedRoom.StreamMode != chat.StreamHistory || len(loadedRoom.TurnHistory) != 1 || loadedRoom.TurnHistory[0].Drafts[0] != "draft" || loadedRoom.PendingPlan == nil || !loadedRoom.PendingPlan.Valid() || loadedRoom.PendingPlan.Content != planContent || loadedRoom.PendingDelegation == nil || !loadedRoom.PendingDelegation.Valid() || len(loadedRoom.PendingDelegation.Tasks) != 1 || !loadedRoom.Workflows["workflow-1"].Valid() || loadedRoom.InputResolutions[1].WorkflowID != "workflow-1" || len(messages) != 1 || messages[0].TurnID != "turn-1" || messages[0].WorkflowMode != chat.WorkflowPlan || messages[0].DelegationPolicy != chat.DelegationAuto || messages[0].Text != "hello" || messages[0].AcceptedPlan == nil || !messages[0].AcceptedPlan.Valid() || messages[0].AcceptedPlan.Content != planContent || len(messages[0].CorrectionEvents) != 1 || messages[0].CorrectionEvents[0].CorrectionSequence != 42 || messages[0].Route == nil || messages[0].Route.MessageID != "external" || len(messages[0].Route.Hops) != 2 {
		t.Fatalf("unexpected round trip: room=%+v messages=%+v", loadedRoom, messages)
	}
	assertMode(t, state, 0o700)
	assertMode(t, filepath.Join(state, room.ID), 0o700)
	assertMode(t, filepath.Join(state, room.ID, roomFile), 0o600)
	assertMode(t, filepath.Join(state, room.ID, transcriptFile), 0o600)
}

func TestLoadRoomRejectsNewerSchema(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	room.SchemaVersion = chat.CurrentRoomSchemaVersion + 1
	if err := value.SaveRoom(room); err != nil {
		t.Fatal(err)
	}
	_, err = value.LoadRoom(room.ID)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("LoadRoom error=%v", err)
	}
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

func TestPeekRoomInUseDoesNotModifyLocks(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := value.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := value.AcquireRoomLock(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inUse, reason, err := value.PeekRoomInUse(room.ID); err != nil || !inUse || !strings.Contains(reason, "live process") {
		t.Fatalf("live lock: inUse=%t reason=%q err=%v", inUse, reason, err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if inUse, reason, err := value.PeekRoomInUse(room.ID); err != nil || inUse || reason != "" {
		t.Fatalf("released lock: inUse=%t reason=%q err=%v", inUse, reason, err)
	}

	path := filepath.Join(value.roomDir(room.ID), roomLockFile)
	stale := []byte(`{"pid":2147483647,"started_at":"2020-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if inUse, reason, err := value.PeekRoomInUse(room.ID); err != nil || inUse || reason != "" {
		t.Fatalf("stale lock: inUse=%t reason=%q err=%v", inUse, reason, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(stale) {
		t.Fatalf("peek modified stale lock: data=%q err=%v", data, err)
	}

	malformed := []byte("not json")
	if err := os.WriteFile(path, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	if inUse, reason, err := value.PeekRoomInUse(room.ID); err == nil || inUse || reason != "" {
		t.Fatalf("malformed lock: inUse=%t reason=%q err=%v", inUse, reason, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(malformed) {
		t.Fatalf("peek modified malformed lock: data=%q err=%v", data, err)
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

func TestDeleteRoomRefusesLiveLockThenClearsResumePointerAndAudits(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	room, err := value.Create(workspace, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.AppendMessage(room.ID, chat.Message{ID: "one", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "hello", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := value.SetResumePointer(workspace, room.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := value.AcquireRoomLock(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.DeleteRoom(room.ID); err == nil || !strings.Contains(err.Error(), "currently open") {
		t.Fatalf("live room deletion error=%v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	info, err := value.DeleteRoom(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != room.ID || info.Workspace != workspace || info.MessageCount != 1 {
		t.Fatalf("delete info=%+v", info)
	}
	if _, err := value.LoadRoom(room.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted room load error=%v", err)
	}
	if pointer, found, err := value.ResumePointer(workspace); err != nil || !found || pointer != "" {
		t.Fatalf("cleared pointer=%q found=%v err=%v", pointer, found, err)
	}
	audit, err := os.ReadFile(filepath.Join(value.Root(), deletionAuditFile))
	if err != nil || !strings.Contains(string(audit), room.ID) || !strings.Contains(string(audit), `"message_count":1`) {
		t.Fatalf("deletion audit=%q err=%v", audit, err)
	}
}

func TestDeleteRoomRejectsInvalidIDAndReapsStaleLock(t *testing.T) {
	value, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.DeleteRoom("../../outside"); err == nil {
		t.Fatal("invalid room id was accepted")
	}
	room, err := value.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	stale := `{"pid":2147483647,"started_at":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(value.roomDir(room.ID), roomLockFile), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := value.DeleteRoom(room.ID); err != nil {
		t.Fatalf("stale lock prevented deletion: %v", err)
	}
}

func assertMode(t *testing.T, path string, wanted os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != wanted {
		t.Fatalf("%s mode=%o want %o", path, info.Mode().Perm(), wanted)
	}
}
