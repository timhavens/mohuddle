package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/timhavens/mohuddle/internal/chat"
)

type previewReadyMsg struct{}
type previewTickMsg struct{}

func waitForPreview(updates <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-updates; !ok {
			return nil
		}
		return previewReadyMsg{}
	}
}

func previewTick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return previewTickMsg{} })
}

func (m *Model) applyPreviews() {
	if m.orchestrator == nil {
		return
	}
	m.ensureLiveMaps()
	for _, p := range m.orchestrator.Previews() {
		// Started/finished events own lifecycle; a late snapshot cannot reopen a turn.
		if m.liveTurnIDs[p.Participant] != p.TurnID || m.liveStates[p.Participant] != "" {
			continue
		}
		if m.streamMode.WithDefault() != chat.StreamStable {
			m.live[p.Participant] = p.Text
			if p.Truncated {
				m.live[p.Participant] += "\n[Provisional preview truncated]"
			}
		}
		m.setActivity(p.Participant, phaseResponding, "streaming response")
	}
	m.previewRefreshes++
}
