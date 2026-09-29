package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

type RoomManager interface {
	RoomNames() (map[string]string, error)
	ResolveRoom(string) (chat.Room, error)
	EnableRoom(string, time.Duration) (string, error)
	DisableRoom(string) error
	ConnectionPath() string
	HasActiveWork() bool
	SetWorkerCounts(map[chat.Participant]int) error
	CloseRoom(string) error
	Overview() []string
}

func (m *Model) ConfigureRoomManager(manager RoomManager, events <-chan room.Event, done <-chan struct{}) {
	m.roomManager, m.roomEvents, m.viewDone = manager, events, done
	if names, err := manager.RoomNames(); err == nil {
		m.roomName = names[m.room.ID]
	}
}

func (m Model) eventSource() <-chan room.Event {
	if m.roomEvents != nil {
		return m.roomEvents
	}
	return m.orchestrator.Events()
}

func (m Model) previewCommand() tea.Cmd {
	if m.viewDone == nil {
		return waitForPreview(m.orchestrator.PreviewUpdates())
	}
	return func() tea.Msg {
		select {
		case <-m.viewDone:
			return nil
		case <-m.orchestrator.PreviewUpdates():
			return previewReadyMsg{}
		}
	}
}
