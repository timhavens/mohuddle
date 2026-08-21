package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

type RoomLister interface {
	ListRooms() ([]chat.Room, error)
}

type ExitAction struct {
	NewRoom  bool
	ResumeID string
}

type activityPhase string

const (
	phaseIdle       activityPhase = "idle"
	phaseQueued     activityPhase = "queued"
	phaseThinking   activityPhase = "thinking"
	phaseResponding activityPhase = "responding"
	phaseTool       activityPhase = "using tool"
	phaseApproval   activityPhase = "needs approval"
	phaseError      activityPhase = "error"
)

type participantActivity struct {
	Phase     activityPhase
	Detail    string
	StartedAt time.Time
}

type Model struct {
	orchestrator *room.Orchestrator
	lister       RoomLister
	room         chat.Room
	messages     []chat.Message
	input        textarea.Model
	viewport     viewport.Model
	width        int
	height       int
	ready        bool
	status       string
	notices      []string
	live         map[chat.Participant]string
	activity     map[chat.Participant]participantActivity
	now          time.Time
	spinnerFrame int
	pending      *agent.ApprovalRequest
	action       ExitAction
	quitting     bool
}

type roomEventMsg struct {
	event room.Event
	open  bool
}

type activityTickMsg time.Time

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Padding(0, 1)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	codexStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	claudeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	systemStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("150"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	busyStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	waitStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	modalStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2)
)

var activitySpinner = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func New(orchestrator *room.Orchestrator, lister RoomLister) Model {
	roomState, messages := orchestrator.Snapshot()
	input := textarea.New()
	input.Placeholder = "Message both agents, or start with @codex / @claude..."
	input.Prompt = "> "
	input.CharLimit = 32 * 1024
	input.SetHeight(3)
	input.Focus()
	now := time.Now()
	return Model{
		orchestrator: orchestrator,
		lister:       lister,
		room:         roomState,
		messages:     messages,
		input:        input,
		viewport:     viewport.New(80, 20),
		status:       "ready",
		live:         map[chat.Participant]string{},
		activity: map[chat.Participant]participantActivity{
			chat.Codex:  {Phase: phaseIdle},
			chat.Claude: {Phase: phaseIdle},
		},
		now: now,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitForRoomEvent(m.orchestrator.Events()), activityTick())
}

func activityTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(now time.Time) tea.Msg {
		return activityTickMsg(now)
	})
}

func waitForRoomEvent(events <-chan room.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return roomEventMsg{event: event, open: ok}
	}
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
		m.resize()
	case roomEventMsg:
		if !value.open {
			m.quitting = true
			return m, tea.Quit
		}
		m.applyRoomEvent(value.event)
		commands = append(commands, waitForRoomEvent(m.orchestrator.Events()))
	case activityTickMsg:
		m.now = time.Time(value)
		m.spinnerFrame++
		commands = append(commands, activityTick())
	case tea.KeyMsg:
		if m.pending != nil {
			if m.handleApprovalKey(value) {
				m.refreshContent()
				return m, tea.Batch(commands...)
			}
			return m, tea.Batch(commands...)
		}
		switch value.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "esc":
			m.orchestrator.Stop()
			m.stopActivities()
			m.status = "stopping active work"
			return m, nil
		case "enter":
			if !value.Alt {
				text := strings.TrimSpace(m.input.Value())
				if text != "" {
					m.input.Reset()
					if command := m.submit(text); command != nil {
						return m, command
					}
				}
				return m, nil
			}
		}
	}

	var command tea.Cmd
	m.input, command = m.input.Update(message)
	commands = append(commands, command)
	if m.ready {
		m.viewport, command = m.viewport.Update(message)
		commands = append(commands, command)
	}
	return m, tea.Batch(commands...)
}

func (m *Model) submit(value string) tea.Cmd {
	if !strings.HasPrefix(value, "/") {
		if err := m.orchestrator.Post(value); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.status = "message sent"
		}
		return nil
	}
	fields := strings.Fields(value)
	command := strings.ToLower(fields[0])
	switch command {
	case "/continue":
		if err := m.orchestrator.Continue(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.queueActivity(chat.Codex)
			m.queueActivity(chat.Claude)
			m.status = "round queued"
		}
	case "/stop":
		m.orchestrator.Stop()
		m.stopActivities()
		m.status = "stopping active work"
	case "/status":
		roomState, _ := m.orchestrator.Snapshot()
		m.addNotice(fmt.Sprintf("room %s\nworkspace: %s\nCodex session: %s\nClaude session: %s",
			roomState.ID, roomState.Workspace, displayID(roomState.Sessions[chat.Codex].ID), displayID(roomState.Sessions[chat.Claude].ID)))
	case "/access":
		roomState, _ := m.orchestrator.Snapshot()
		var lines []string
		for _, grant := range roomState.Grants {
			lines = append(lines, fmt.Sprintf("%s  %-10s  %s", grant.Participant, grant.Mode, grant.Path))
		}
		m.addNotice("Access grants:\n" + strings.Join(lines, "\n"))
	case "/revoke":
		if len(fields) < 2 {
			m.addNotice("usage: /revoke [@codex|@claude|@all] PATH")
			break
		}
		var participant chat.Participant
		pathStart := 1
		switch strings.ToLower(fields[1]) {
		case "@codex":
			participant = chat.Codex
			pathStart = 2
		case "@claude":
			participant = chat.Claude
			pathStart = 2
		case "@all":
			pathStart = 2
		}
		if len(fields) <= pathStart {
			m.addNotice("usage: /revoke [@codex|@claude|@all] PATH")
			break
		}
		if err := m.orchestrator.RevokeGrant(strings.Join(fields[pathStart:], " "), participant); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.addNotice("Access grant revoked")
		}
	case "/rooms":
		if m.lister == nil {
			m.addNotice("Room listing is unavailable")
			break
		}
		rooms, err := m.lister.ListRooms()
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		var lines []string
		for _, roomState := range rooms {
			lines = append(lines, fmt.Sprintf("%s  %s  %s", roomState.ID, roomState.UpdatedAt.Local().Format("2006-01-02 15:04"), roomState.Workspace))
		}
		if len(lines) == 0 {
			lines = append(lines, "No saved rooms")
		}
		m.addNotice(strings.Join(lines, "\n"))
	case "/new":
		m.action.NewRoom = true
		m.quitting = true
		return tea.Quit
	case "/resume":
		if len(fields) != 2 {
			m.addNotice("usage: /resume ROOM_ID")
			break
		}
		m.action.ResumeID = fields[1]
		m.quitting = true
		return tea.Quit
	case "/help":
		m.addNotice("Commands: /continue /stop /status /access /revoke [@agent] PATH /rooms /new /resume ID /help /quit\nTargets: @codex MESSAGE or @claude MESSAGE\nKeys: Enter sends, Alt+Enter adds a line, Esc stops active work")
	case "/quit", "/exit":
		m.quitting = true
		return tea.Quit
	default:
		m.addNotice("unknown command; use /help")
	}
	return nil
}

func (m *Model) applyRoomEvent(event room.Event) {
	switch event.Type {
	case room.EventMessage:
		if event.Message != nil {
			m.messages = append(m.messages, *event.Message)
			switch {
			case event.Message.Author == chat.User && event.Message.Kind == chat.MessageText:
				if event.Message.Target.ValidAgent() {
					m.queueActivity(event.Message.Target)
				} else {
					m.queueActivity(chat.Codex)
					m.queueActivity(chat.Claude)
				}
			case event.Message.Author.ValidAgent() && event.Message.Kind == chat.MessageTool:
				m.setActivity(event.Message.Author, phaseTool, event.Message.Text)
			case event.Message.Author.ValidAgent() && event.Message.Kind == chat.MessageText:
				delete(m.live, event.Message.Author)
				m.finishActivity(event.Message.Author, "response posted")
			}
			m.status = fmt.Sprintf("%s posted", event.Message.Author)
		}
	case room.EventTurnStarted:
		m.setActivity(event.Participant, phaseThinking, "waiting for model response")
		m.status = fmt.Sprintf("%s is thinking", event.Participant)
	case room.EventTurnFinished:
		m.finishActivity(event.Participant, "")
	case room.EventAgent:
		if event.AgentEvent == nil {
			break
		}
		agentEvent := event.AgentEvent
		switch agentEvent.Type {
		case agent.EventDelta:
			m.live[agentEvent.Agent] += agentEvent.Text
			m.setActivity(agentEvent.Agent, phaseResponding, "streaming response")
			m.status = fmt.Sprintf("%s is responding", agentEvent.Agent)
		case agent.EventTool:
			detail := strings.TrimSpace(agentEvent.Text)
			if detail == "" {
				detail = "tool activity"
			}
			m.setActivity(agentEvent.Agent, phaseTool, detail)
			m.status = fmt.Sprintf("%s used a tool", agentEvent.Agent)
		case agent.EventStatus:
			m.setActivity(agentEvent.Agent, phaseThinking, agentEvent.Text)
			m.status = agentEvent.Text
		case agent.EventApproval:
			m.pending = agentEvent.Approval
			detail := "waiting for your decision"
			if agentEvent.Approval != nil && strings.TrimSpace(agentEvent.Approval.Title) != "" {
				detail = agentEvent.Approval.Title
			}
			m.setActivity(agentEvent.Agent, phaseApproval, detail)
			m.status = "approval required"
		}
	case room.EventRoundDone:
		m.finishBusyActivities()
		m.status = event.Text
	case room.EventError:
		if event.Err != nil {
			m.addNotice(errorStyle.Render(event.Err.Error()))
			m.errorActivity(event.Participant, event.Err.Error())
			m.status = "agent error"
		}
	}
	m.refreshContent()
}

func (m *Model) handleApprovalKey(key tea.KeyMsg) bool {
	var decision agent.ApprovalDecision
	switch strings.ToLower(key.String()) {
	case "y":
		decision = agent.ApproveOnce
	case "a":
		decision = agent.ApproveSession
	case "b":
		if m.pending.Kind != "directory_access" {
			return false
		}
		decision = agent.ApproveBoth
	case "n":
		decision = agent.Deny
	case "x", "esc":
		decision = agent.CancelTurn
	default:
		return false
	}
	select {
	case m.pending.Response <- decision:
	default:
	}
	participant := m.pending.Agent
	m.pending = nil
	if decision == agent.CancelTurn {
		m.finishActivity(participant, "turn stopped")
	} else {
		m.setActivity(participant, phaseThinking, "approval answered; resuming")
	}
	m.status = "approval answered"
	return true
}

func (m *Model) queueActivity(participant chat.Participant) {
	if !participant.ValidAgent() {
		return
	}
	m.ensureActivityMap()
	m.activity[participant] = participantActivity{
		Phase:     phaseQueued,
		Detail:    "waiting for turn",
		StartedAt: m.activityTime(),
	}
}

func (m *Model) setActivity(participant chat.Participant, phase activityPhase, detail string) {
	if !participant.ValidAgent() {
		return
	}
	m.ensureActivityMap()
	current := m.activity[participant]
	if current.StartedAt.IsZero() || !isBusyPhase(current.Phase) {
		current.StartedAt = m.activityTime()
	}
	current.Phase = phase
	current.Detail = cleanActivityDetail(detail)
	m.activity[participant] = current
}

func (m *Model) finishActivity(participant chat.Participant, detail string) {
	if !participant.ValidAgent() {
		return
	}
	m.ensureActivityMap()
	current := m.activity[participant]
	if current.Phase == phaseError {
		return
	}
	current.Phase = phaseIdle
	current.StartedAt = time.Time{}
	if strings.TrimSpace(detail) != "" {
		current.Detail = cleanActivityDetail(detail)
	}
	m.activity[participant] = current
}

func (m *Model) errorActivity(participant chat.Participant, detail string) {
	if !participant.ValidAgent() {
		return
	}
	m.ensureActivityMap()
	m.activity[participant] = participantActivity{Phase: phaseError, Detail: cleanActivityDetail(detail)}
}

func (m *Model) finishBusyActivities() {
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		if isBusyPhase(m.activity[participant].Phase) {
			m.finishActivity(participant, "round complete")
		}
	}
}

func (m *Model) stopActivities() {
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		if isBusyPhase(m.activity[participant].Phase) {
			m.finishActivity(participant, "stopped")
		}
	}
}

func (m *Model) ensureActivityMap() {
	if m.activity == nil {
		m.activity = make(map[chat.Participant]participantActivity, 2)
	}
}

func (m *Model) activityTime() time.Time {
	if m.now.IsZero() {
		m.now = time.Now()
	}
	return m.now
}

func isBusyPhase(phase activityPhase) bool {
	switch phase {
	case phaseQueued, phaseThinking, phaseResponding, phaseTool, phaseApproval:
		return true
	default:
		return false
	}
}

func (m *Model) resize() {
	headerHeight := 4
	inputHeight := 4
	statusHeight := 2
	modalHeight := 0
	if m.pending != nil {
		modalHeight = 8
	}
	viewportHeight := m.height - headerHeight - inputHeight - statusHeight - modalHeight
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.viewport.Width = max(20, m.width)
	m.viewport.Height = viewportHeight
	m.input.SetWidth(max(10, m.width-2))
	m.ready = true
	m.refreshContent()
}

func (m *Model) refreshContent() {
	if !m.ready {
		return
	}
	width := max(20, m.viewport.Width-2)
	var value strings.Builder
	for _, message := range m.messages {
		label := authorStyle(message.Author).Render(strings.ToUpper(string(message.Author)))
		if message.Target.ValidAgent() {
			label += dimStyle.Render(" → " + string(message.Target))
		}
		timeLabel := dimStyle.Render(message.CreatedAt.Local().Format("15:04:05"))
		fmt.Fprintf(&value, "%s %s\n", label, timeLabel)
		bodyStyle := lipgloss.NewStyle().Width(width)
		if message.Kind == chat.MessageTool {
			bodyStyle = bodyStyle.Foreground(lipgloss.Color("244")).Italic(true)
		}
		value.WriteString(bodyStyle.Render(message.Text))
		value.WriteString("\n\n")
	}
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		if text := strings.TrimSpace(publicLiveText(m.live[participant])); text != "" {
			fmt.Fprintf(&value, "%s %s\n%s\n\n", authorStyle(participant).Render(strings.ToUpper(string(participant))), dimStyle.Render("streaming…"), lipgloss.NewStyle().Width(width).Render(text))
		}
	}
	for _, notice := range m.notices {
		fmt.Fprintf(&value, "%s\n\n", lipgloss.NewStyle().Width(width).Render(notice))
	}
	m.viewport.SetContent(value.String())
	m.viewport.GotoBottom()
}

func publicLiveText(value string) string {
	if index := strings.Index(value, "<!--"); index >= 0 {
		return value[:index]
	}
	return value
}

func (m *Model) addNotice(value string) {
	m.notices = append(m.notices, value)
	if len(m.notices) > 8 {
		m.notices = m.notices[len(m.notices)-8:]
	}
	m.refreshContent()
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	header := headerStyle.Render("MOHUDDLE") + " " + dimStyle.Render(shortID(m.room.ID)+"  "+m.room.Workspace)
	parts := []string{header, m.activityView(), m.viewport.View()}
	if m.pending != nil {
		description := m.pending.Description
		if m.pending.Path != "" {
			description += "\nPath: " + m.pending.Path + " (" + string(m.pending.Mode) + ")"
		}
		choices := "[y] once  [a] this room  [n] deny  [x] stop turn"
		if m.pending.Kind == "directory_access" {
			choices = "[y] once  [a] this agent/room  [b] both agents/room  [n] deny  [x] stop"
		}
		modal := fmt.Sprintf("%s\n%s\n\n%s", lipgloss.NewStyle().Bold(true).Render(m.pending.Title), description, dimStyle.Render(choices))
		parts = append(parts, modalStyle.Width(max(20, m.width-6)).Render(modal))
	}
	parts = append(parts, m.input.View(), dimStyle.Render("status: "+m.status+"   /help for commands"))
	return strings.Join(parts, "\n")
}

func (m Model) activityView() string {
	lines := make([]string, 0, 2)
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		lines = append(lines, m.activityLine(participant))
	}
	return strings.Join(lines, "\n")
}

func (m Model) activityLine(participant chat.Participant) string {
	activity := m.activity[participant]
	if activity.Phase == "" {
		activity.Phase = phaseIdle
	}

	icon := "○"
	phaseStyle := dimStyle
	switch {
	case activity.Phase == phaseApproval:
		icon = "?"
		phaseStyle = waitStyle
	case activity.Phase == phaseError:
		icon = "!"
		phaseStyle = errorStyle
	case isBusyPhase(activity.Phase):
		icon = activitySpinner[m.spinnerFrame%len(activitySpinner)]
		phaseStyle = busyStyle
	}

	label := authorStyle(participant).Render(fmt.Sprintf("%-7s", strings.ToUpper(string(participant))))
	line := fmt.Sprintf("%s %s %s", phaseStyle.Render(icon), label, phaseStyle.Render(string(activity.Phase)))
	if isBusyPhase(activity.Phase) && !activity.StartedAt.IsZero() {
		line += dimStyle.Render("  " + formatElapsed(m.now.Sub(activity.StartedAt)))
	}
	if activity.Detail != "" && m.width >= 48 {
		limit := max(12, m.width-38)
		line += dimStyle.Render("  · " + truncateActivityDetail(activity.Detail, limit))
	}
	return line
}

func cleanActivityDetail(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateActivityDetail(value string, limit int) string {
	value = cleanActivityDetail(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit < 2 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func formatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int(elapsed / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds %= 60
	if minutes < 60 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	hours := minutes / 60
	minutes %= 60
	return fmt.Sprintf("%dh%02dm", hours, minutes)
}

func (m Model) Action() ExitAction { return m.action }

func authorStyle(author chat.Participant) lipgloss.Style {
	switch author {
	case chat.User:
		return userStyle
	case chat.Codex:
		return codexStyle
	case chat.Claude:
		return claudeStyle
	default:
		return systemStyle
	}
}

func displayID(value string) string {
	if value == "" {
		return "not started"
	}
	return value
}

func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
