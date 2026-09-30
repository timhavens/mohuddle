//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/chatgpt"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

func managerForTest(t *testing.T) (*managedRooms, *managedRuntime, string) {
	t.Helper()
	root := testutil.ShortTempDir(t)
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := appsettings.Open(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	opts := options{agyBinary: writeDoctorHelperExecutable(t, root, "agy")}
	m, err := newManagedRooms(s, root, opts, prefs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	runtime, err := m.Open(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartGateway(runtime); err != nil {
		t.Fatal(err)
	}
	return m, runtime, state.ID
}

func TestManagedRoomsKeepRuntimesAndGatewayIndependent(t *testing.T) {
	m, first, firstID := managerForTest(t)
	state, err := m.Create(m.workspace, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Open(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	revisited, err := m.Open(firstID)
	if err != nil || revisited != first {
		t.Fatal("switching views replaced the live runtime", err)
	}
	path, err := m.EnableRoom(firstID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := chatgpt.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var view api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: "second-conversation", Room: state.ID}, &view); err != nil {
		t.Fatal(err)
	}
	if err := m.SetWorkerCounts(map[chat.Participant]int{chat.Codex: 1}); err != nil {
		t.Fatal(err)
	}
	if state, _ := second.api.service.ChatGPTStatus(); !state.Connected {
		t.Fatal("worker change lost room attachment")
	}
	m.refreshOverview(time.Now())
	if overview := strings.Join(m.Overview(), "\n"); !strings.Contains(overview, "Room 1") || !strings.Contains(overview, "Room 2") {
		t.Fatal(overview)
	}
	if err := m.CloseRoom(firstID); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Doctor(t.Context()); err != nil {
		t.Fatal("closing original room broke gateway", err)
	}
	var read api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID}, &read); err != nil || read.RoomID != state.ID {
		t.Fatal("other room lost access", err)
	}
	if inUse, _, err := m.PeekRoomInUse(firstID); err != nil || inUse {
		t.Fatal("closed room lock not released", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("manager grant survived app exit", err)
	}
	if inUse, _, err := m.PeekRoomInUse(state.ID); err != nil || inUse {
		t.Fatal("app exit left second room running", err)
	}
}

func TestFailedSecondManagerDoesNotRevokeRunningManager(t *testing.T) {
	m, _, id := managerForTest(t)
	path, err := m.EnableRoom(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newManagedRooms(m.Store, m.workspace, m.opts, m.preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Create(m.workspace, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := other.Open(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.StartGateway(runtime); err == nil {
		t.Fatal("second manager shared live manager socket")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	bridge, err := chatgpt.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := bridge.Doctor(ctx); err != nil {
		t.Fatal("failed startup broke active manager", err)
	}
}

func TestManagerRestoresConversationRoomAfterAppRestart(t *testing.T) {
	m, _, initialID := managerForTest(t)
	path, err := m.EnableRoom(initialID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := chatgpt.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var joined api.ChatGPTView
	input := api.ManagedJoinRequest{ClientKey: "persisted-conversation", OperationID: "new-room"}
	if err := bridge.Call(t.Context(), "chatgpt.create_room", input, &joined); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newManagedRooms(m.Store, m.workspace, m.opts, m.preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	initial, err := restarted.Open(initialID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.StartGateway(initial); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.EnableRoom(initialID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var restored api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: input.ClientKey}, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.RoomID != joined.RoomID || restored.RoomName != joined.RoomName || restored.ParticipationID == joined.ParticipationID {
		t.Fatalf("restart lost selection or reused stale capability: %+v", restored)
	}
	var choices api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: "brand-new-conversation"}, &choices); err != nil || !choices.SelectionRequired {
		t.Fatal("new chat silently inherited old selection", err)
	}
}

func TestResetRoomDisconnectsOldChatAndPreservesOtherRoom(t *testing.T) {
	m, first, id := managerForTest(t)
	otherState, err := m.Create(m.workspace, 1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Open(otherState.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.EnableRoom(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := chatgpt.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var old, peer api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: "reset-me-conversation", Room: id}, &old); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: "keep-me-conversation", Room: otherState.ID}, &peer); err != nil {
		t.Fatal(err)
	}
	archive, err := m.ResetRoom(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.Open(id)
	if err != nil || fresh == first {
		t.Fatal("runtime not replaced", err)
	}
	unchanged, err := m.Open(otherState.ID)
	if err != nil || unchanged != other {
		t.Fatal("other runtime replaced", err)
	}
	var read api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.read", api.ChatGPTReadRequest{ParticipationID: old.ParticipationID}, &read); err == nil {
		t.Fatal("stale attachment accepted")
	}
	if err := bridge.Call(t.Context(), "chatgpt.read", api.ChatGPTReadRequest{ParticipationID: peer.ParticipationID}, &read); err != nil {
		t.Fatal("other room disconnected", err)
	}
	if _, err := m.EnableRoom(id, time.Hour); err != nil {
		t.Fatal(err)
	}
	var joined api.ChatGPTView
	if err := bridge.Call(t.Context(), "chatgpt.join", api.ManagedJoinRequest{ClientKey: "fresh-chat-conversation", Room: id}, &joined); err != nil {
		t.Fatal(err)
	}
	if joined.ParticipationID == old.ParticipationID {
		t.Fatal("old participation reused")
	}
}
