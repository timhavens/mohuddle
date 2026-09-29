package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

func streamingModel(count int) Model {
	m := Model{room: chat.NewRoom("room", "/workspace", 3, time.Now()), viewport: viewport.New(120, 30), ready: true, following: true, activity: map[chat.Participant]participantActivity{}, now: time.Now()}
	for i := 0; i < count; i++ {
		m.messages = append(m.messages, chat.Message{ID: fmt.Sprint(i), Sequence: uint64(i + 1), Author: chat.User, Kind: chat.MessageText, Text: strings.Repeat("A retained room message. ", 5), CreatedAt: time.Unix(int64(i), 0)})
	}
	return m
}

func TestStreamingDoesNotRebuildLongTranscript(t *testing.T) {
	for _, mode := range []chat.StreamMode{chat.StreamStable, chat.StreamLive, chat.StreamHistory} {
		m := streamingModel(5000)
		m.streamMode = mode
		m.refreshContent()
		before := m.transcriptRebuilds
		rendered := m.messageRenders
		for i := 0; i < 1500; i++ {
			p := chat.Participant([]string{"codex", "codex-1", "codex-2"}[i%3])
			m.applyRoomEvent(room.Event{Type: room.EventAgent, Participant: p, TurnID: string(p), AgentEvent: &agent.Event{Type: agent.EventDelta, Agent: p, Text: "x"}})
		}
		if m.transcriptRebuilds != before || m.messageRenders != rendered {
			t.Fatal("streaming rebuilt transcript", mode)
		}
		m.refreshContent()
		if m.messageRenders != rendered {
			t.Fatal("unchanged messages were rerendered")
		}
		m.messages[0].Text = "changed"
		m.refreshContent()
		if m.messageRenders != rendered+1 {
			t.Fatal("cache did not invalidate edited message")
		}
		m.viewport.Width = 100
		m.refreshContent()
		if m.messageRenders != rendered+5001 {
			t.Fatal("width change did not invalidate cache")
		}
	}
}

func BenchmarkLongRoomStreaming(b *testing.B) {
	m := streamingModel(5000)
	m.refreshContent()
	e := room.Event{Type: room.EventAgent, Participant: chat.Codex, TurnID: "turn", AgentEvent: &agent.Event{Type: agent.EventDelta, Agent: chat.Codex, Text: "x"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.applyRoomEvent(e)
	}
}

func TestTypingDoesNotRebuildLongTranscript(t *testing.T) {
	m := streamingModel(5000)
	m.width, m.height, m.input = 120, 50, newComposerInput()
	m.input.Focus()
	m.resize()
	before := m.transcriptRebuilds
	for _, ch := range "a new question about this room" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		m = next.(Model)
	}
	if m.input.Value() != "a new question about this room" || m.transcriptRebuilds != before {
		t.Fatal("typing rebuilt history or lost input", m.transcriptRebuilds-before, m.input.Value())
	}
	m.following = false
	m.viewport.SetYOffset(12)
	m.input.SetValue("line one\nline two")
	m.resizeInput()
	if m.viewport.YOffset != 12 || m.transcriptRebuilds != before {
		t.Fatal("composer resize lost scroll position or rebuilt history")
	}
	m.following = true
	m.resizeInput()
	if !m.viewport.AtBottom() {
		t.Fatal("following did not preserve bottom position")
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	m = next.(Model)
	if m.transcriptRebuilds != before+1 || m.viewport.Width != 100 {
		t.Fatal("terminal width change did not rebuild history")
	}
}
