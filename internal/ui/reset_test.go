package ui

import (
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
	"testing"
)

type resetTestManager struct{ RoomManager }

func TestResetRequiresPreviewAndExplicitConfirmation(t *testing.T) {
	s, _ := store.New(t.TempDir())
	state, _ := s.Create(t.TempDir(), 1)
	o, err := room.New(state, nil, s, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	m := New(o, s)
	m.roomManager = resetTestManager{}
	m.submit("/reset confirm")
	if m.action.ResetRoom {
		t.Fatal("reset without preview")
	}
	m.submit("/reset")
	if m.action.ResetRoom {
		t.Fatal("preview reset room")
	}
	m.submit("/reset typo")
	if m.action.ResetRoom {
		t.Fatal("invalid confirmation reset room")
	}
	if cmd := m.submit("/reset confirm"); cmd == nil || !m.action.ResetRoom || !m.quitting {
		t.Fatal("confirmed reset not handed to manager")
	}
}
