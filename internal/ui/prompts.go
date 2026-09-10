package ui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

type promptViewer struct {
	snapshot room.PromptSnapshot
	viewport viewport.Model
	input    bool
	preview  bool
	title    string
	text     string
	native   chat.Participant
}

const promptUsage = "usage: /prompt [@agent] [preview|native] | /prompt default | /prompt room [TEXT|clear] | /prompt @agent TEXT|clear\nUse -- TEXT after room or @agent to save literal text such as clear, preview, or native."

// splitPromptArgument consumes only the command word, preserving spacing and
// newlines inside user-written prompts instead of rebuilding strings.Fields.
func splitPromptArgument(value string) (string, string) {
	value = strings.TrimSpace(value)
	if index := strings.IndexFunc(value, unicode.IsSpace); index >= 0 {
		return value[:index], strings.TrimSpace(value[index:])
	}
	return value, ""
}

func (m *Model) promptCommand(value string) {
	_, args := splitPromptArgument(value)
	target, text := splitPromptArgument(args)
	if target == "" {
		m.showPrompt(chat.Codex, false)
		return
	}
	switch strings.ToLower(target) {
	case "default":
		if text != "" {
			m.addNotice(errorStyle.Render(promptUsage))
			return
		}
		m.showPromptText("MOHUDDLE BUILT-IN PROMPT", agent.RoomProtocolPrompt)
		return
	case "preview":
		if text != "" {
			m.addNotice(errorStyle.Render(promptUsage))
			return
		}
		m.showPrompt(chat.Codex, true)
		return
	case "native":
		if text != "" {
			m.addNotice(errorStyle.Render(promptUsage))
			return
		}
		m.showNativePrompt(chat.Codex)
		return
	case "room":
		if text == "" {
			state, _ := m.orchestrator.Snapshot()
			text = state.RoomPrompt
			if text == "" {
				text = "No custom room prompt is set. Agents without individual overrides use MoHuddle's built-in guidance.\n\n" + agent.RoomProtocolPrompt
			}
			m.showPromptText("SAVED ROOM PROMPT", text)
			return
		}
		m.setPrompt(chat.System, text)
		return
	}
	participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(target, "@")))
	if !ok {
		m.addNotice(errorStyle.Render(promptUsage))
		return
	}
	if text == "" || strings.EqualFold(text, "preview") {
		m.showPrompt(participant, text != "")
		return
	}
	if strings.EqualFold(text, "native") {
		m.showNativePrompt(participant)
		return
	}
	m.setPrompt(participant, text)
}

func (m *Model) setPrompt(participant chat.Participant, text string) {
	word, rest := splitPromptArgument(text)
	if word == "--" {
		if rest == "" {
			m.addNotice(errorStyle.Render(promptUsage))
			return
		}
		text = rest
	} else if strings.EqualFold(text, "clear") {
		text = ""
	}
	if err := m.orchestrator.SetPrompt(participant, text); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	scope := "Room prompt"
	detail := "Agents without individual overrides use this guidance."
	if participant != chat.System {
		scope = "Prompt override for @" + string(participant)
		detail = "This replaces the room's custom guidance for this agent."
	}
	action := " saved. "
	if text == "" {
		action = " cleared. "
		detail = "MoHuddle's default guidance now applies unless an individual override is set."
		if participant != chat.System {
			detail = "This agent now inherits the room prompt."
		}
	}
	m.syncRoom()
	m.addNotice(scope + action + detail + " Changes apply to subsequent turns. Use /prompt [@agent] preview to review the current configuration.")
}

func (m *Model) showPrompt(participant chat.Participant, preview bool) {
	snapshot, err := m.orchestrator.Prompt(participant, preview)
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.promptViewer = &promptViewer{snapshot: snapshot, viewport: viewport.New(80, 20), preview: preview}
	m.resizePromptViewer()
}

func (m *Model) showPromptText(title, text string) {
	m.promptViewer = &promptViewer{title: title, text: text, viewport: viewport.New(80, 20)}
	m.resizePromptViewer()
}

func (m *Model) showNativePrompt(participant chat.Participant) {
	if _, err := m.orchestrator.Prompt(participant, true); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	state, _ := m.orchestrator.Snapshot()
	model := m.orchestrator.EffectiveSettings()[participant].Model
	if reported := state.ParticipantRuntime[participant].ReportedModel; reported != "" {
		model = reported
	}
	report := agent.InspectNativePrompts(participant, state.Workspace, model)
	lines := append([]string(nil), report.Notes...)
	if len(report.Sources) == 0 {
		lines = append(lines, "", "No instruction files found in the standard locations checked.")
	}
	for _, source := range report.Sources {
		lines = append(lines, "", source.Path, source.Note, "", source.Text)
	}
	m.showPromptText("NATIVE INSTRUCTION SOURCES · @"+string(participant), strings.Join(lines, "\n"))
	m.promptViewer.native = participant
	m.resizePromptViewer()
}

func (m *Model) handlePromptKey(key tea.KeyMsg) tea.Cmd {
	switch strings.ToLower(key.String()) {
	case "esc":
		m.promptViewer = nil
		return nil
	case "ctrl+c":
		m.quitting = true
		return tea.Quit
	case "tab", "shift+tab":
		if m.promptViewer.title != "" {
			return nil
		}
		m.promptViewer.input = !m.promptViewer.input
		m.resizePromptViewer()
		m.promptViewer.viewport.GotoTop()
	case "r":
		if m.promptViewer.native.ValidAgent() {
			m.showNativePrompt(m.promptViewer.native)
			return nil
		}
		if m.promptViewer.title != "" {
			if m.promptViewer.title == "SAVED ROOM PROMPT" {
				m.promptCommand("/prompt room")
			}
			return nil
		}
		snapshot, err := m.orchestrator.Prompt(m.promptViewer.snapshot.Participant, m.promptViewer.preview)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			m.promptViewer = nil
			return nil
		}
		m.promptViewer.snapshot = snapshot
		m.resizePromptViewer()
		m.promptViewer.viewport.GotoTop()
	case "n":
		if m.promptViewer.snapshot.Participant.ValidAgent() {
			m.showNativePrompt(m.promptViewer.snapshot.Participant)
		}
	case "home", "ctrl+home":
		m.promptViewer.viewport.GotoTop()
	case "end", "ctrl+end":
		m.promptViewer.viewport.GotoBottom()
	default:
		var command tea.Cmd
		m.promptViewer.viewport, command = m.promptViewer.viewport.Update(key)
		return command
	}
	return nil
}

func (m *Model) resizePromptViewer() {
	if m.promptViewer == nil {
		return
	}
	viewer := m.promptViewer
	viewer.viewport.Width = max(1, m.width)
	header, footer := m.promptViewerFrame()
	viewer.viewport.Height = max(1, m.height-strings.Count(header, "\n")-strings.Count(footer, "\n")-2)
	text := viewer.snapshot.Request.SystemPrompt
	if viewer.snapshot.Request.PromptOverride != "" {
		text = "MOHUDDLE PROMPT OVERRIDE\n" + viewer.snapshot.Request.PromptOverride + "\n\nMOHUDDLE COORDINATION INSTRUCTIONS\n" + text
	}
	if viewer.title != "" {
		text = viewer.text
	} else if viewer.input {
		text = viewer.snapshot.Request.Prompt
		if len(viewer.snapshot.Request.Attachments) > 0 {
			text += "\n\nATTACHMENTS (sent separately)"
			for _, attachment := range viewer.snapshot.Request.Attachments {
				text += fmt.Sprintf("\n%s: %s", attachment.Kind, attachment.Path)
			}
		}
	}
	// Preserve complete prompt text and whitespace while preventing transcript
	// control sequences from changing the terminal. Wrap even long tokens.
	viewer.viewport.SetContent(wrapPromptText(text, viewer.viewport.Width))
}

func wrapPromptText(text string, width int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(text))
	return ansi.Hardwrap(strings.ReplaceAll(text, "\t", "    "), max(1, width), true)
}

func (m Model) promptViewerFrame() (string, string) {
	viewer := m.promptViewer
	if viewer.title != "" {
		header := viewer.title + "\nRoom prompts apply to all agents; individual prompts replace the room's custom guidance."
		if viewer.native.ValidAgent() {
			header = viewer.title + "\nRead-only view of provider files. MoHuddle overrides never modify these files."
		}
		footer := "↑/↓ PgUp/PgDn scroll · Home/End · r refresh · Alt+M select text · Esc close"
		return wrapPromptText(header, m.width), wrapPromptText(footer, m.width)
	}
	snapshot := viewer.snapshot
	section := "MOHUDDLE INSTRUCTIONS"
	if viewer.input {
		section = "TURN INPUT AND CUSTOM GUIDANCE"
	}
	state := "LATEST CAPTURED REQUEST"
	if snapshot.Preview {
		state = "PREVIEW · not sent"
	} else if snapshot.Active {
		state = "ACTIVE TURN · captured request"
	}
	lines := []string{fmt.Sprintf("PROMPT · @%s · %s", snapshot.Participant, section), state}
	if snapshot.Preview {
		lines = append(lines, "Current settings and pending context; workflow instructions are assigned when a turn starts.")
	} else {
		lines = append(lines, "Captured: "+snapshot.CapturedAt.Local().Format("2006-01-02 15:04:05")+" · turn "+snapshot.TurnID)
	}
	lines = append(lines,
		"MoHuddle adapter request only. Native provider instructions and earlier session context are not included.",
		"Use n for native instruction files. Captures last until MoHuddle exits.")
	if snapshot.Request.PromptOverride != "" {
		note := "Override: native base-prompt replacement; MoHuddle's coordination protocol still applies."
		if snapshot.Participant.Provider() == chat.Agy {
			note = "Override: turn guidance only; AGY's print transport cannot replace its native base prompt."
		}
		lines = append(lines, note)
	}
	header := wrapPromptText(strings.Join(lines, "\n"), m.width)
	footer := wrapPromptText("Tab instructions/input · n native files · ↑/↓ PgUp/PgDn scroll · Home/End · r refresh · Alt+M select text · Esc close", m.width)
	return header, footer
}

func (m Model) promptViewerView() string {
	header, footer := m.promptViewerFrame()
	return header + "\n" + m.promptViewer.viewport.View() + "\n" + dimStyle.Render(footer)
}
