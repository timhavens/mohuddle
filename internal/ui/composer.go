package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	compactPasteThreshold = 1000
	maxPastedTextChars    = 1024 * 1024
	maxComposerHistory    = 200
)

type composerStore interface {
	LoadComposerHistory(string) ([]chat.ComposerHistoryEntry, error)
	SaveComposerHistory(string, []chat.ComposerHistoryEntry) error
	SaveAttachment(string, chat.Attachment, []byte) (chat.Attachment, error)
}

type noticeEntry struct {
	Text      string
	CreatedAt time.Time
}

type commandSuggestion struct {
	Name        string
	Description string
}

var commandSuggestions = []commandSuggestion{
	{"/ask", "independent answers from selected agents"},
	{"/round", "read-only group discussion and synthesis"},
	{"/core", "configure core peers and failover"},
	{"/moderator", "show or change the room moderator"},
	{"/agents", "show the room roster"},
	{"/workers", "configure auxiliary AI workers"},
	{"/delegate", "hand a subtask to an auxiliary worker"},
	{"/roster", "schedule or cancel future roster changes"},
	{"/join", "bring an agent into the room"},
	{"/leave", "remove an agent from the room"},
	{"/continue", "continue a paused room workflow"},
	{"/steer", "replace active work with new direction"},
	{"/stop", "stop active agent work"},
	{"/progress", "set compact, detailed, or hidden work status"},
	{"/details", "toggle behind-the-scenes activity"},
	{"/sound", "toggle the AI-finished terminal sound"},
	{"/speak", "control spoken responses"},
	{"/voice", "set an agent voice"},
	{"/voices", "list available voices"},
	{"/status", "show room and agent status"},
	{"/settings", "show effective agent settings"},
	{"/models", "list models for an agent"},
	{"/model", "set a model override"},
	{"/effort", "set reasoning effort"},
	{"/permissions", "set an access profile"},
	{"/inherit", "restore personal defaults"},
	{"/access", "show filesystem grants"},
	{"/revoke", "remove a filesystem grant"},
	{"/rooms", "list saved rooms"},
	{"/new", "start a new room"},
	{"/resume", "resume a saved room"},
	{"/help", "show all commands and keys"},
	{"/quit", "leave MoHuddle"},
}

func (m *Model) currentComposerEntry() chat.ComposerHistoryEntry {
	return chat.ComposerHistoryEntry{
		Text:        m.input.Value(),
		Pastes:      append([]string(nil), m.pastes...),
		Attachments: append([]chat.Attachment(nil), m.attachments...),
		CreatedAt:   time.Now().UTC(),
	}
}

func (m *Model) restoreComposerEntry(entry chat.ComposerHistoryEntry) {
	m.input.SetValue(entry.Text)
	m.input.CursorEnd()
	m.pastes = append([]string(nil), entry.Pastes...)
	m.attachments = append([]chat.Attachment(nil), entry.Attachments...)
	m.resize()
}

func (m *Model) resetComposer() {
	m.input.Reset()
	m.pastes = nil
	m.attachments = nil
	m.historyIndex = len(m.history)
	m.historyDraft = nil
	m.suggestionIndex = 0
	m.resize()
}

func (m *Model) addHistory(entry chat.ComposerHistoryEntry) {
	if strings.TrimSpace(entry.Text) == "FULL ACCESS" && len(entry.Pastes) == 0 && len(entry.Attachments) == 0 {
		return
	}
	if len(m.history) > 0 && sameComposerEntry(m.history[len(m.history)-1], entry) {
		m.historyIndex = len(m.history)
		return
	}
	m.history = append(m.history, entry)
	if len(m.history) > maxComposerHistory {
		m.history = m.history[len(m.history)-maxComposerHistory:]
	}
	m.historyIndex = len(m.history)
	if m.composerStore != nil {
		if err := m.composerStore.SaveComposerHistory(m.room.ID, m.history); err != nil {
			m.addNotice(errorStyle.Render("Could not save input history: " + err.Error()))
		}
	}
}

func sameComposerEntry(left, right chat.ComposerHistoryEntry) bool {
	if left.Text != right.Text || len(left.Pastes) != len(right.Pastes) || len(left.Attachments) != len(right.Attachments) {
		return false
	}
	for index := range left.Pastes {
		if left.Pastes[index] != right.Pastes[index] {
			return false
		}
	}
	for index := range left.Attachments {
		if left.Attachments[index].ID != right.Attachments[index].ID {
			return false
		}
	}
	return true
}

func (m *Model) browseHistory(delta int) bool {
	if len(m.history) == 0 {
		return false
	}
	if m.historyIndex < 0 || m.historyIndex > len(m.history) {
		m.historyIndex = len(m.history)
	}
	if m.historyIndex == len(m.history) && delta < 0 {
		draft := m.currentComposerEntry()
		m.historyDraft = &draft
	}
	next := m.historyIndex + delta
	if next < 0 {
		next = 0
	}
	if next > len(m.history) {
		next = len(m.history)
	}
	if next == m.historyIndex {
		return true
	}
	m.historyIndex = next
	if next == len(m.history) {
		if m.historyDraft != nil {
			m.restoreComposerEntry(*m.historyDraft)
		} else {
			m.restoreComposerEntry(chat.ComposerHistoryEntry{})
		}
		return true
	}
	m.restoreComposerEntry(m.history[next])
	return true
}

func (m *Model) addPastedText(value string) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.TrimSpace(value) == "" {
		return
	}
	current := 0
	for _, pasted := range m.pastes {
		current += utf8.RuneCountInString(pasted)
	}
	remaining := maxPastedTextChars - current
	if remaining <= 0 {
		m.addNotice(errorStyle.Render("Pasted content limit reached (1 MiB of text)"))
		return
	}
	runes := []rune(value)
	if len(runes) > remaining {
		runes = runes[:remaining]
		m.addNotice(waitStyle.Render("Pasted content was truncated at the 1 MiB composer limit"))
	}
	m.pastes = append(m.pastes, string(runes))
	m.resize()
}

func compactPastedText(value string) bool {
	return strings.ContainsAny(value, "\r\n") || utf8.RuneCountInString(value) > compactPasteThreshold
}

func (m Model) composedText() string {
	parts := make([]string, 0, 1+len(m.pastes))
	if text := strings.TrimSpace(m.input.Value()); text != "" {
		parts = append(parts, text)
	}
	for _, pasted := range m.pastes {
		if text := strings.TrimSpace(pasted); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 && len(m.attachments) > 0 {
		for index := range m.attachments {
			parts = append(parts, fmt.Sprintf("[Image #%d attached]", index+1))
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m *Model) removeLastComposerItem() bool {
	if strings.TrimSpace(m.input.Value()) != "" {
		return false
	}
	if len(m.attachments) > 0 {
		m.attachments = m.attachments[:len(m.attachments)-1]
		m.resize()
		return true
	}
	if len(m.pastes) > 0 {
		m.pastes = m.pastes[:len(m.pastes)-1]
		m.resize()
		return true
	}
	return false
}

func (m Model) composerItemsView() string {
	if len(m.pastes) == 0 && len(m.attachments) == 0 {
		return ""
	}
	items := make([]string, 0, len(m.pastes)+len(m.attachments))
	for index, pasted := range m.pastes {
		items = append(items, fmt.Sprintf("[Pasted Content %d · %d chars]", index+1, utf8.RuneCountInString(pasted)))
	}
	for index, attachment := range m.attachments {
		dimensions := ""
		if attachment.Width > 0 && attachment.Height > 0 {
			dimensions = fmt.Sprintf(" · %d×%d", attachment.Width, attachment.Height)
		}
		items = append(items, fmt.Sprintf("[Image #%d%s]", index+1, dimensions))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Render(strings.Join(items, "  "))
}

func (m Model) composerItemsHeight() int {
	if len(m.pastes) == 0 && len(m.attachments) == 0 {
		return 0
	}
	return 1
}

func (m Model) matchingCommands() []commandSuggestion {
	if m.suggestionsHidden {
		return nil
	}
	value := strings.TrimSpace(m.input.Value())
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t\r\n") {
		return nil
	}
	var matches []commandSuggestion
	for _, suggestion := range commandSuggestions {
		if strings.HasPrefix(suggestion.Name, strings.ToLower(value)) {
			matches = append(matches, suggestion)
		}
	}
	if len(matches) > 6 {
		matches = matches[:6]
	}
	return matches
}

func (m Model) suggestionsView() string {
	matches := m.matchingCommands()
	if len(matches) == 0 {
		return ""
	}
	lines := make([]string, 0, len(matches))
	for index, suggestion := range matches {
		prefix := "  "
		style := dimStyle
		if index == m.suggestionIndex%len(matches) {
			prefix = "› "
			style = userStyle
		}
		lines = append(lines, style.Render(fmt.Sprintf("%s%-13s %s", prefix, suggestion.Name, suggestion.Description)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) completeSuggestion() bool {
	matches := m.matchingCommands()
	if len(matches) == 0 {
		return false
	}
	selected := matches[m.suggestionIndex%len(matches)]
	m.input.SetValue(selected.Name + " ")
	m.input.CursorEnd()
	m.suggestionIndex = 0
	m.resize()
	return true
}

func supportsConversationAttachments(value string) bool {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "/") {
		return true
	}
	command := strings.ToLower(strings.Fields(trimmed)[0])
	return command == "/ask" || command == "/once" || command == "/round"
}

func (m *Model) handleTranscriptKey(value string) bool {
	if !m.ready {
		return false
	}
	switch value {
	case "pgup":
		m.viewport.PageUp()
	case "pgdown":
		m.viewport.PageDown()
	case "ctrl+up":
		m.viewport.LineUp(1)
	case "ctrl+down":
		m.viewport.LineDown(1)
	case "ctrl+home":
		m.viewport.GotoTop()
	case "ctrl+end":
		m.viewport.GotoBottom()
	default:
		return false
	}
	m.following = m.viewport.AtBottom()
	if m.following {
		m.unseen = 0
	}
	return true
}

func (m Model) composerParticipants() []chat.Participant {
	value := strings.TrimSpace(m.input.Value())
	fields := strings.Fields(value)
	if len(fields) > 0 {
		command := strings.ToLower(fields[0])
		if command == "/ask" || command == "/once" || command == "/round" {
			var selected []chat.Participant
			for _, field := range fields[1:] {
				if !strings.HasPrefix(field, "@") {
					break
				}
				participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(field, "@")))
				if !ok {
					break
				}
				selected = append(selected, participant)
			}
			if len(selected) > 0 {
				return selected
			}
			return m.room.PresentAgents()
		}
	}
	if strings.HasPrefix(value, "@") {
		token := value
		if index := strings.IndexAny(token, " \t\r\n,:"); index >= 0 {
			token = token[:index]
		}
		token = strings.TrimRight(token, "?!")
		if participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(token, "@"))); ok {
			return []chat.Participant{participant}
		}
	}
	if m.orchestrator != nil {
		active := m.orchestrator.CoreStatus().Active
		if len(active) > 0 {
			return active
		}
	}
	moderator := m.room.Moderator
	if moderator.ValidAgent() {
		return []chat.Participant{moderator}
	}
	return []chat.Participant{chat.Codex}
}

func (m Model) contextFooter() string {
	selected := make(map[chat.Participant]bool)
	for _, participant := range m.composerParticipants() {
		selected[participant] = true
	}
	settings := m.currentSettings()
	coreParticipants := []chat.Participant{chat.Codex, chat.Claude}
	if m.orchestrator != nil {
		status := m.orchestrator.CoreStatus()
		coreParticipants = status.Active
		if len(coreParticipants) == 0 {
			coreParticipants = status.Policy.Preferred
		}
	}
	contexts := make([]string, 0, len(coreParticipants))
	for _, participant := range coreParticipants {
		labelStyle := dimStyle.Bold(true)
		if selected[participant] {
			labelStyle = authorStyle(participant)
		}
		contexts = append(contexts,
			labelStyle.Render(strings.ToUpper(string(participant)))+
				dimStyle.Render(" · "+compactSettings(settings[participant])),
		)
	}
	workspace := m.room.Workspace
	if workspace == "" {
		workspace = "."
	}
	if m.width < 120 && len(workspace) > 28 {
		workspace = "…/" + filepath.Base(workspace)
	}
	line := strings.Join(contexts, dimStyle.Render("  │  ")) + dimStyle.Render("  │  "+workspace)
	if m.speech != nil {
		line = m.speechBadge() + "  " + line
	}
	return line
}

func (m Model) keyFooter() string {
	status := m.status
	if m.unseen > 0 {
		status = fmt.Sprintf("%d new · Ctrl+End", m.unseen)
	}
	mouseMode := "scroll"
	if !m.mouseCaptured {
		mouseMode = "select"
	}
	keys := "Enter send · Alt+Enter newline · ↑ history · PgUp scroll · Ctrl+V paste · Alt+M mouse=" + mouseMode + " · / commands"
	if m.width < 86 {
		keys = "Enter send · ↑ history · PgUp scroll · Ctrl+V paste · / help"
	}
	return dimStyle.Render(keys + "   " + status)
}
