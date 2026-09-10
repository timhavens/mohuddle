package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
)

func newPromptTestModel(t *testing.T) Model {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, s, rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Participant("codex-1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	m := New(o, s)
	m.width, m.height = 100, 32
	m.resize()
	return m
}

func TestPromptCommandsSetClearAndPreserveMultilineText(t *testing.T) {
	m := newPromptTestModel(t)
	m.submit("/prompt room Write clearly.\n  Use examples and `code`.  Preserve spacing.")
	m.submit("/prompt @codex-1 Focus on tests.\nDo not expand scope.")
	state, messages := m.orchestrator.Snapshot()
	if state.RoomPrompt != "Write clearly.\n  Use examples and `code`.  Preserve spacing." || state.AgentPrompts["codex-1"] != "Focus on tests.\nDo not expand scope." || len(messages) != 0 {
		t.Fatalf("prompt commands: room=%q overrides=%v messages=%d", state.RoomPrompt, state.AgentPrompts, len(messages))
	}
	m.submit("/prompt @codex-1 preview")
	if m.promptViewer == nil || m.promptViewer.snapshot.Request.PromptOverride != state.AgentPrompts["codex-1"] {
		t.Fatal("worker override was not visible")
	}
	m.submit("/prompt @codex-1 clear")
	m.submit("/prompt @codex-1 preview")
	if m.promptViewer.snapshot.Request.PromptOverride != state.RoomPrompt {
		t.Fatal("worker did not inherit room prompt")
	}
	m.submit("/prompt @codex-1 Set priorities and focus on tests.")
	state, _ = m.orchestrator.Snapshot()
	if state.AgentPrompts["codex-1"] != "Set priorities and focus on tests." {
		t.Fatal("natural prompt text was mistaken for a subcommand")
	}
	m.submit("/prompt @codex-1 -- clear")
	state, _ = m.orchestrator.Snapshot()
	if state.AgentPrompts["codex-1"] != "clear" {
		t.Fatal("literal reserved prompt text was lost")
	}
	m.submit("/prompt room clear")
	state, _ = m.orchestrator.Snapshot()
	if state.RoomPrompt != "" || state.AgentPrompts["codex-1"] != "clear" {
		t.Fatal("clearing room prompt changed individual override")
	}
}

func TestPromptViewerNavigationDoesNotSubmitOrModifyComposer(t *testing.T) {
	m := newPromptTestModel(t)
	m.input.SetValue("unfinished message")
	m.submit("/prompt @codex preview")
	if m.promptViewer == nil || !strings.Contains(m.View(), "PREVIEW") {
		t.Fatal("viewer did not open")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if !m.promptViewer.input || !strings.Contains(m.View(), "TURN INPUT") {
		t.Fatal("Tab did not switch sections")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	if !m.promptViewer.viewport.AtBottom() {
		t.Fatal("End did not reach complete prompt")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.promptViewer != nil || m.input.Value() != "unfinished message" {
		t.Fatal("closing viewer lost composer state")
	}
	_, messages := m.orchestrator.Snapshot()
	if len(messages) != 0 {
		t.Fatal("viewer added content to AI transcript")
	}
	m.submit("/prompt default")
	if !strings.Contains(m.promptViewer.text, "private control marker") {
		t.Fatal("built-in MoHuddle prompt was unavailable")
	}
}

func TestPromptDisplayWrapsLongTokensAndStripsTerminalControls(t *testing.T) {
	text := "\x1b[2J" + strings.Repeat("長", 70) + "\x00\nlast line"
	wrapped := wrapPromptText(text, 40)
	if strings.ContainsAny(wrapped, "\x1b\x00") || !strings.Contains(wrapped, "last line") || strings.Count(wrapped, "長") != 70 {
		t.Fatal("display lost text or retained terminal controls")
	}
	for _, line := range strings.Split(wrapped, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Fatal("long prompt was clipped rather than wrapped")
		}
	}
}
