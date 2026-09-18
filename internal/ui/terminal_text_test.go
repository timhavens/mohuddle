package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/x/ansi"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestTerminalTextRemovesCommandsAndPreservesUnicode(t *testing.T) {
	for _, attack := range []string{
		"\x1b]52;c;YXVkaXQ=\a", "\x1b]52;c;YXVkaXQ=\x1b\\",
		"\x1b[2J", "\x1b[8m", "\x1bPignored\x1b\\", "\x1b_apc\x1b\\",
		"\a\r\b\x00\x7f", "\u202e\u2066\u2069",
	} {
		got := terminalText("safe" + attack + " text\n\t世界 👩‍💻")
		if got != "safe text\n\t世界 👩‍💻" {
			t.Errorf("sanitized result=%q", got)
		}
	}
}

func TestTerminalFramePreservesStylingButRejectsTerminalOperations(t *testing.T) {
	style := "\x1b[1;38;2;12;34;56m"
	text := style + "hello 世界 👩‍💻" + "\x1b[0m\n\t"
	attack := "\x1b]52;c;YXVkaXQ=\a\x1b[2J\x1b[?25l\x1bPignored\x1b\\\a"
	if got := terminalFrame(text + attack); got != text {
		t.Fatalf("frame=%q want=%q", got, text)
	}
	// Colon-separated true-color styles and incomplete trailing escapes stay safe.
	text = "\x1b[38:2::12:34:56mhello\x1b[0m"
	got := terminalFrame(text + "\x1b]52;c;unfinished")
	if ansi.Strip(got) != "hello" || strings.Contains(got, "]52") {
		t.Fatalf("frame=%q", got)
	}
}

func TestUntrustedUIContentCannotEmitTerminalCommands(t *testing.T) {
	payload := "before\x1b]52;c;YXVkaXQ=\a\x1b[8mafter"
	model := Model{
		ready: true, width: 120, viewport: viewport.New(116, 25),
		turnViewport: viewport.New(116, 25), input: textarea.New(),
		room:     chat.Room{Workspace: payload},
		messages: []chat.Message{{ID: "audit", Sequence: 1, Author: chat.Claude, Kind: chat.MessageText, Text: payload, CreatedAt: time.Now()}},
		notices:  []noticeEntry{{Text: payload, CreatedAt: time.Now()}},
		pending:  &agent.ApprovalRequest{Title: payload, Description: payload, Path: payload},
	}
	model.refreshContent()
	assertSafe := func(name, rendered string) {
		t.Helper()
		if strings.Contains(rendered, "]52;") || strings.Contains(rendered, "\x1b[8m") || strings.Contains(rendered, "\a") {
			t.Errorf("%s retained injected controls: %q", name, rendered)
		}
		if !strings.Contains(ansi.Strip(rendered), "beforeafter") {
			t.Errorf("%s lost visible message text", name)
		}
	}
	assertSafe("transcript", model.viewport.View())
	assertSafe("full view including approval and footer", model.View())
	if model.messages[0].Text != payload || model.pending.Description != payload {
		t.Fatal("rendering altered stored content or the actual approval request")
	}
	model.streamMode = chat.StreamHistory
	model.liveTurnIDs = map[chat.Participant]string{chat.Claude: "turn"}
	model.live = map[chat.Participant]string{chat.Claude: payload}
	assertSafe("live response", model.liveResponseView())
	model.turns = []chat.TurnRecord{{Participant: chat.Claude, Drafts: []string{payload}, Tools: []string{payload}}}
	model.refreshTurnViewport()
	assertSafe("turn history", model.turnViewport.View())
}
