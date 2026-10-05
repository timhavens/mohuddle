package ui

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
)

type roomsTestManager struct {
	RoomManager
	*store.Store
	locks map[string]*store.RoomLock
}

func (m *roomsTestManager) RoomNames() (map[string]string, error) {
	return m.Store.RoomNames()
}

func (m *roomsTestManager) ResolveRoom(selector string) (chat.Room, error) {
	return m.Store.ResolveRoom(selector)
}

func (m *roomsTestManager) CloseRoom(selector string) error {
	state, err := m.ResolveRoom(selector)
	if err != nil {
		return err
	}
	return m.locks[state.ID].Release()
}

func (m *roomsTestManager) Overview() []string {
	return []string{"Rooms overview"}
}

func roomDeletionTestModel(t *testing.T) (Model, *roomsTestManager, []chat.Room) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := &roomsTestManager{Store: s, locks: map[string]*store.RoomLock{}}
	var states []chat.Room
	for index := 0; index < 6; index++ {
		state, err := s.Create(t.TempDir(), 1)
		if err != nil {
			t.Fatal(err)
		}
		// Assign each room's number as it is created.
		if _, err := s.RoomNames(); err != nil {
			t.Fatal(err)
		}
		lock, err := s.AcquireRoomLock(state.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Release() })
		manager.locks[state.ID] = lock
		states = append(states, state)
	}
	o, err := room.New(states[4], nil, s, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	m := New(o, manager)
	m.ConfigureRoomManager(manager, nil, nil)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(Model), manager, states
}

func submitRoomCommand(t *testing.T, m *Model, command string) {
	t.Helper()
	m.input.SetValue(command)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	*m = updated.(Model)
}

func TestRoomDeletionFromOverviewKeepsOtherRoomsOpen(t *testing.T) {
	m, manager, states := roomDeletionTestModel(t)
	for _, target := range []string{"room4", "room6"} {
		selected, err := manager.ResolveRoom(target)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range []struct{ command, notice string }{
			{"/rooms close " + target, "Room closed; other rooms continue working."},
			{"/rooms delete " + target, "Delete room " + selected.ID + "?"},
			{"/rooms delete " + target + " confirm", "Deleted room " + selected.ID},
		} {
			submitRoomCommand(t, &m, "/rooms")
			if !strings.Contains(m.View(), "Rooms overview") {
				t.Fatal("room overview did not open")
			}
			submitRoomCommand(t, &m, step.command)
			if !strings.Contains(m.View(), step.notice) {
				t.Fatalf("%s hid its result:\n%s", step.command, m.View())
			}
			if m.quitting || m.action.ResumeID != "" || m.room.ID != states[4].ID {
				t.Fatal("deletion required leaving room5")
			}
		}
		if _, err := manager.LoadRoom(selected.ID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target room remains: %v", err)
		}
	}
	for _, index := range []int{0, 1, 2, 4} {
		if _, err := manager.LoadRoom(states[index].ID); err != nil {
			t.Fatalf("kept room%d was removed: %v", index+1, err)
		}
		if inUse, _, err := manager.PeekRoomInUse(states[index].ID); err != nil || !inUse {
			t.Fatalf("kept room%d was closed: inUse=%v err=%v", index+1, inUse, err)
		}
	}
}

func TestRoomDeletionCurrentRoomExplainsSwitchAndClose(t *testing.T) {
	m, manager, states := roomDeletionTestModel(t)
	submitRoomCommand(t, &m, "/rooms")
	submitRoomCommand(t, &m, "/rooms delete room5")
	view := m.View()
	if !strings.Contains(view, "Switch to another room") || !strings.Contains(view, "/rooms close") {
		t.Fatalf("current-room error does not explain recovery:\n%s", view)
	}
	if _, err := manager.LoadRoom(states[4].ID); err != nil {
		t.Fatalf("current room was deleted: %v", err)
	}
}

func TestRoomDeletionConfirmStillRequiresPreview(t *testing.T) {
	m, manager, _ := roomDeletionTestModel(t)
	submitRoomCommand(t, &m, "/rooms close room4")
	submitRoomCommand(t, &m, "/rooms")
	submitRoomCommand(t, &m, "/rooms delete room4 confirm")
	selected, err := manager.ResolveRoom("room4")
	if err != nil {
		t.Fatalf("confirmation without a preview deleted the room: %v", err)
	}
	if !strings.Contains(m.View(), "Delete room "+selected.ID+"?") {
		t.Fatalf("required preview is hidden:\n%s", m.View())
	}
}

func TestRoomDeletionOpenRoomExplainsCloseBeforeConfirmation(t *testing.T) {
	m, manager, _ := roomDeletionTestModel(t)
	submitRoomCommand(t, &m, "/rooms")
	submitRoomCommand(t, &m, "/rooms delete room4")
	selected, err := manager.ResolveRoom("room4")
	if err != nil {
		t.Fatal(err)
	}
	view := m.View()
	if !strings.Contains(view, "is still open") || !strings.Contains(view, "/rooms close "+selected.ID) {
		t.Fatalf("open-room error does not explain recovery:\n%s", view)
	}
	if m.roomDeleteConfirm != "" {
		t.Fatal("offered deletion confirmation for an open room")
	}
	if inUse, _, err := manager.PeekRoomInUse(selected.ID); err != nil || !inUse {
		t.Fatalf("preview closed the room: inUse=%v err=%v", inUse, err)
	}
}
