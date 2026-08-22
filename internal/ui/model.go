package ui

import (
	"context"
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
	appsettings "github.com/timhavens/mohuddle/internal/settings"
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
	phaseAway       activityPhase = "away"
)

type participantActivity struct {
	Phase     activityPhase
	Detail    string
	StartedAt time.Time
}

type settingsChange struct {
	Field        string
	Participants []chat.Participant
	Value        string
	Default      bool
}

type Model struct {
	orchestrator     *room.Orchestrator
	lister           RoomLister
	room             chat.Room
	messages         []chat.Message
	input            textarea.Model
	viewport         viewport.Model
	width            int
	height           int
	ready            bool
	status           string
	notices          []string
	live             map[chat.Participant]string
	activity         map[chat.Participant]participantActivity
	now              time.Time
	spinnerFrame     int
	pending          *agent.ApprovalRequest
	approvalQueue    []*agent.ApprovalRequest
	showDetails      bool
	fullConfirmation *settingsChange
	action           ExitAction
	quitting         bool
}

type roomEventMsg struct {
	event room.Event
	open  bool
}

type activityTickMsg time.Time

type modelsMsg struct {
	participant chat.Participant
	models      []agent.ModelOption
	err         error
}

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Padding(0, 1)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	userStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	codexStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	claudeStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	agyStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	copilotStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("141"))
	systemStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("150"))
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	busyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	waitStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	modalStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2)
)

var activitySpinner = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func New(orchestrator *room.Orchestrator, lister RoomLister) Model {
	roomState, messages := orchestrator.Snapshot()
	input := textarea.New()
	input.Placeholder = "Message the room, or target @codex / @claude / @agy / @copilot..."
	input.Prompt = "> "
	input.CharLimit = 32 * 1024
	input.SetHeight(3)
	input.Focus()
	now := time.Now()
	model := Model{
		orchestrator: orchestrator,
		lister:       lister,
		room:         roomState,
		messages:     messages,
		input:        input,
		viewport:     viewport.New(80, 20),
		status:       "ready",
		live:         map[chat.Participant]string{},
		activity:     map[chat.Participant]participantActivity{},
		showDetails:  orchestrator.DetailsVisible(),
		now:          now,
	}
	for _, participant := range orchestrator.Participants() {
		phase := phaseAway
		if roomState.Present(participant) {
			phase = phaseIdle
		}
		model.activity[participant] = participantActivity{Phase: phase}
	}
	if roomState.Conflict != nil {
		model.status = "conflict requires your direction"
	}
	return model
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
	case modelsMsg:
		if value.err != nil {
			m.addNotice(errorStyle.Render(value.err.Error()))
			m.status = "model catalog failed"
		} else {
			m.addNotice(formatModels(value.participant, value.models))
			m.status = "model catalog loaded"
		}
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
	if m.fullConfirmation != nil {
		change := m.fullConfirmation
		m.fullConfirmation = nil
		if value != "FULL ACCESS" {
			m.addNotice("Full access was not enabled; acknowledgement must match FULL ACCESS exactly")
			m.status = "full access cancelled"
			return nil
		}
		if err := m.orchestrator.AcknowledgeFullAccess(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return nil
		}
		if err := m.applySettingsChange(*change); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.addNotice("Full-machine access acknowledged and saved")
			m.status = "settings updated"
		}
		m.syncRoom()
		return nil
	}
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
	case "/ask", "/once":
		prompt := strings.TrimSpace(value[len(fields[0]):])
		if prompt == "" {
			m.addNotice(errorStyle.Render("usage: " + command + " MESSAGE"))
			break
		}
		if err := m.orchestrator.Ask(prompt); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.queuePresentActivities()
			m.status = "one-shot question sent"
		}
	case "/continue":
		if err := m.orchestrator.Continue(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoom()
			m.queuePresentActivities()
			m.status = "round queued"
		}
	case "/stop":
		m.orchestrator.Stop()
		m.stopActivities()
		m.status = "stopping active work"
	case "/details":
		visible, err := parseDetails(fields, m.showDetails)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		if err := m.orchestrator.SetDetailsVisible(visible); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.showDetails = visible
		state := "off"
		if visible {
			state = "on"
		}
		m.status = "details " + state
		m.addNotice("Behind-the-scenes details are now " + state + ".")
		m.refreshContent()
	case "/status":
		roomState, _ := m.orchestrator.Snapshot()
		configured := m.orchestrator.EffectiveSettings()
		lines := []string{fmt.Sprintf("room %s\nworkspace: %s", roomState.ID, roomState.Workspace)}
		for _, participant := range m.orchestrator.Participants() {
			presence := "away"
			if roomState.Present(participant) {
				presence = "present"
			}
			lines = append(lines, fmt.Sprintf("%s: %s; %s; session %s", strings.ToUpper(string(participant)), presence, settingsSummary(configured[participant]), displayID(roomState.Sessions[participant].ID)))
		}
		m.addNotice(strings.Join(lines, "\n"))
	case "/agents":
		m.showAgents()
	case "/join", "/leave":
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: " + command + " @codex|@claude|@agy|@copilot|@all"))
			break
		}
		participants, err := parseSettingsParticipants(fields, 1)
		if err != nil {
			m.addNotice(errorStyle.Render("usage: " + command + " @codex|@claude|@agy|@copilot|@all"))
			break
		}
		if strings.EqualFold(fields[1], "@all") {
			participants = m.orchestrator.Participants()
		}
		present := command == "/join"
		for _, participant := range participants {
			if err := m.orchestrator.SetPresence(participant, present); err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
		}
		m.syncRoomMetadata()
		for _, participant := range participants {
			if m.room.Present(participant) {
				m.finishActivity(participant, "joined room")
			} else {
				m.activity[participant] = participantActivity{Phase: phaseAway, Detail: "not participating"}
			}
		}
		m.status = "room roster updated"
	case "/settings":
		m.showSettings()
	case "/models":
		participants, err := parseSettingsParticipants(fields, 1)
		if err != nil || len(participants) != 1 {
			m.addNotice(errorStyle.Render("usage: /models @codex|@claude|@agy|@copilot"))
			break
		}
		m.status = "loading model catalog"
		return loadModels(m.orchestrator, participants[0])
	case "/model", "/effort", "/permissions":
		change, err := parseSettingsChange(command, fields)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		if change.Field == "permissions" && change.Value == string(chat.PermissionFull) && !m.orchestrator.FullAccessAcknowledged() {
			m.fullConfirmation = &change
			m.addNotice("WARNING: full mode gives the selected agent(s) unrestricted host filesystem and network access with no provider approvals. Type FULL ACCESS and press Enter to acknowledge, or type anything else to cancel.")
			m.status = "awaiting full-access acknowledgement"
			break
		}
		if err := m.applySettingsChange(change); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoom()
			m.status = "settings updated"
		}
	case "/inherit":
		participants, err := parseSettingsParticipants(fields, 1)
		if err != nil {
			m.addNotice(errorStyle.Render("usage: /inherit @agent|@all"))
			break
		}
		for _, participant := range participants {
			if err := m.orchestrator.InheritAgentSettings(participant); err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
		}
		m.syncRoom()
		m.status = "room now inherits personal defaults"
	case "/access":
		roomState, _ := m.orchestrator.Snapshot()
		var lines []string
		for _, grant := range roomState.Grants {
			lines = append(lines, fmt.Sprintf("%s  %-10s  %s", grant.Participant, grant.Mode, grant.Path))
		}
		m.addNotice("Access grants:\n" + strings.Join(lines, "\n"))
	case "/revoke":
		if len(fields) < 2 {
			m.addNotice("usage: /revoke [@agent|@all] PATH")
			break
		}
		var participant chat.Participant
		pathStart := 1
		if strings.HasPrefix(fields[1], "@") {
			if strings.EqualFold(fields[1], "@all") {
				pathStart = 2
			} else if parsed, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(fields[1], "@"))); ok {
				participant = parsed
				pathStart = 2
			}
		}
		if len(fields) <= pathStart {
			m.addNotice("usage: /revoke [@agent|@all] PATH")
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
		m.addNotice("Commands: /ask MESSAGE /agents /join @agent /leave @agent /continue /stop /details [on|off] /status /settings /models @agent /model [default] @agent VALUE /effort [default] @agent VALUE /permissions [default] @agent PROFILE /inherit @agent /access /revoke [@agent] PATH /rooms /new /resume ID /help /quit\n/ask (alias /once) gets one independent response per present agent with no peer-review wave. Agents: @codex, @claude, @agy, @copilot, or @all. Profiles: read-only, workspace, full.\nKeys: Enter sends, Alt+Enter adds a line, Esc stops active work")
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
			if event.Message.Author == chat.User && m.room.Conflict != nil {
				roomState, _ := m.orchestrator.Snapshot()
				m.room = roomState
			}
			switch {
			case event.Message.Author == chat.User && event.Message.Kind == chat.MessageText:
				if event.Message.Target.ValidAgent() {
					m.queueActivity(event.Message.Target)
				} else {
					m.queuePresentActivities()
				}
			case event.Message.Author.ValidAgent() && event.Message.Kind == chat.MessageTool:
				m.setActivity(event.Message.Author, phaseTool, event.Message.Text)
			case event.Message.Author.ValidAgent() && (event.Message.Kind == chat.MessageText || event.Message.Kind == chat.MessageInterrupted):
				delete(m.live, event.Message.Author)
				detail := "response posted"
				if event.Message.Kind == chat.MessageInterrupted {
					detail = "draft interrupted"
				}
				m.finishActivity(event.Message.Author, detail)
			}
			m.status = fmt.Sprintf("%s posted", event.Message.Author)
		}
	case room.EventWaveStarted:
		for _, participant := range event.Participants {
			m.queueActivity(participant)
		}
		if m.showDetails && strings.TrimSpace(event.Text) != "" {
			m.status = event.Text
		} else {
			m.status = "workflow running"
		}
	case room.EventTurnStarted:
		m.setActivity(event.Participant, phaseThinking, "waiting for model response")
		m.status = fmt.Sprintf("%s is thinking", event.Participant)
	case room.EventTurnFinished:
		m.discardApprovals(event.Participant)
		delete(m.live, event.Participant)
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
			if m.showDetails {
				m.status = fmt.Sprintf("%s used a tool", agentEvent.Agent)
			} else {
				m.status = "agents are working"
			}
		case agent.EventStatus:
			m.setActivity(agentEvent.Agent, phaseThinking, agentEvent.Text)
			if m.showDetails {
				m.status = agentEvent.Text
			} else {
				m.status = "agents are working"
			}
		case agent.EventApproval:
			m.enqueueApproval(agentEvent.Approval)
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
	case room.EventConflict:
		m.syncRoom()
		m.status = "conflict requires your direction"
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
	m.advanceApproval()
	return true
}

func (m *Model) enqueueApproval(request *agent.ApprovalRequest) {
	if request == nil {
		return
	}
	if m.pending == nil {
		m.pending = request
		return
	}
	m.approvalQueue = append(m.approvalQueue, request)
}

func (m *Model) advanceApproval() {
	if m.pending != nil || len(m.approvalQueue) == 0 {
		return
	}
	m.pending = m.approvalQueue[0]
	m.approvalQueue = m.approvalQueue[1:]
	m.setActivity(m.pending.Agent, phaseApproval, m.pending.Title)
	m.status = "approval required"
}

func (m *Model) discardApprovals(participant chat.Participant) {
	if m.pending != nil && m.pending.Agent == participant {
		m.pending = nil
	}
	filtered := m.approvalQueue[:0]
	for _, request := range m.approvalQueue {
		if request.Agent != participant {
			filtered = append(filtered, request)
		}
	}
	m.approvalQueue = filtered
	m.advanceApproval()
}

func (m *Model) queueActivity(participant chat.Participant) {
	if !participant.ValidAgent() || !m.room.Present(participant) {
		return
	}
	m.ensureActivityMap()
	m.activity[participant] = participantActivity{
		Phase:     phaseQueued,
		Detail:    "waiting for turn",
		StartedAt: m.activityTime(),
	}
}

func (m *Model) queuePresentActivities() {
	for _, participant := range m.room.PresentAgents() {
		m.queueActivity(participant)
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
	for _, participant := range m.activityParticipants() {
		if isBusyPhase(m.activity[participant].Phase) {
			m.finishActivity(participant, "round complete")
		}
	}
}

func (m *Model) stopActivities() {
	for _, participant := range m.activityParticipants() {
		if isBusyPhase(m.activity[participant].Phase) {
			m.finishActivity(participant, "stopped")
		}
	}
}

func (m *Model) ensureActivityMap() {
	if m.activity == nil {
		m.activity = make(map[chat.Participant]participantActivity, len(chat.Agents()))
	}
}

func (m Model) activityParticipants() []chat.Participant {
	if m.orchestrator != nil {
		return m.orchestrator.Participants()
	}
	return chat.Agents()
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
	headerHeight := len(m.activityParticipants()) + 2
	inputHeight := 4
	statusHeight := 2
	modalHeight := 0
	if m.pending != nil || m.fullConfirmation != nil || m.room.Conflict != nil {
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
		if message.Kind == chat.MessageTool && !m.showDetails {
			continue
		}
		label := authorStyle(message.Author).Render(strings.ToUpper(string(message.Author)))
		if message.Target.ValidAgent() {
			label += dimStyle.Render(" → " + string(message.Target))
		}
		if message.Kind == chat.MessageInterrupted {
			label += waitStyle.Render(" (interrupted)")
		}
		timeLabel := dimStyle.Render(message.CreatedAt.Local().Format("15:04:05"))
		fmt.Fprintf(&value, "%s %s\n", label, timeLabel)
		bodyStyle := lipgloss.NewStyle().Width(width)
		if message.Kind == chat.MessageTool {
			bodyStyle = bodyStyle.Foreground(lipgloss.Color("244")).Italic(true)
		} else if message.Kind == chat.MessageInterrupted {
			bodyStyle = bodyStyle.Foreground(lipgloss.Color("214")).Italic(true)
		}
		value.WriteString(bodyStyle.Render(message.Text))
		value.WriteString("\n\n")
	}
	for _, participant := range m.activityParticipants() {
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
	configured := m.currentSettings()
	for _, participant := range m.room.PresentAgents() {
		if configured[participant].Permissions == chat.PermissionFull {
			header += " " + errorStyle.Bold(true).Render("FULL ACCESS")
			break
		}
	}
	parts := []string{header, m.activityView(), m.viewport.View()}
	if m.pending != nil {
		description := m.pending.Description
		if m.pending.Path != "" {
			description += "\nPath: " + m.pending.Path + " (" + string(m.pending.Mode) + ")"
		}
		choices := "[y] once  [a] this room  [n] deny  [x] stop turn"
		if m.pending.Kind == "directory_access" {
			choices = "[y] once  [a] this agent/room  [b] all agents/room  [n] deny  [x] stop"
		}
		modal := fmt.Sprintf("%s\n%s\n\n%s", lipgloss.NewStyle().Bold(true).Render(m.pending.Title), description, dimStyle.Render(choices))
		parts = append(parts, modalStyle.Width(max(20, m.width-6)).Render(modal))
	}
	if m.fullConfirmation != nil {
		parts = append(parts, modalStyle.Width(max(20, m.width-6)).Render(errorStyle.Render("FULL MACHINE ACCESS\nType FULL ACCESS below to save this acknowledgement, or anything else to cancel.")))
	} else if m.room.Conflict != nil {
		modal := "CONFLICT\n" + conflictSummary(m.room.Conflict) + "\n\nSend your direction or use /continue."
		parts = append(parts, modalStyle.Width(max(20, m.width-6)).Render(waitStyle.Render(modal)))
	}
	parts = append(parts, m.input.View(), dimStyle.Render("status: "+m.status+"   /help for commands"))
	return strings.Join(parts, "\n")
}

func (m Model) activityView() string {
	participants := m.activityParticipants()
	lines := make([]string, 0, len(participants))
	for _, participant := range participants {
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
	if !m.showDetails {
		phase := activity.Phase
		if phase == phaseThinking || phase == phaseResponding || phase == phaseTool {
			phase = "working"
		}
		line := fmt.Sprintf("%s %s %s", phaseStyle.Render(icon), label, phaseStyle.Render(string(phase)))
		if isBusyPhase(activity.Phase) && !activity.StartedAt.IsZero() {
			line += dimStyle.Render("  " + formatElapsed(m.now.Sub(activity.StartedAt)))
		}
		return line
	}
	configured := m.currentSettings()[participant]
	line := fmt.Sprintf("%s %s %s %s", phaseStyle.Render(icon), label, phaseStyle.Render(string(activity.Phase)), dimStyle.Render("["+compactSettings(configured)+"]"))
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

func parseDetails(fields []string, current bool) (bool, error) {
	if len(fields) == 1 {
		return !current, nil
	}
	if len(fields) != 2 {
		return false, fmt.Errorf("usage: /details [on|off]")
	}
	switch strings.ToLower(fields[1]) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("usage: /details [on|off]")
	}
}

func conflictSummary(conflict *chat.ConflictState) string {
	if conflict == nil {
		return "material disagreement"
	}
	parts := make([]string, 0, len(conflict.Reasons))
	for _, participant := range chat.Agents() {
		if reason := strings.TrimSpace(conflict.Reasons[participant]); reason != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", participant, reason))
		}
	}
	reason := strings.Join(parts, "; ")
	if reason == "" {
		reason = strings.TrimSpace(conflict.Reason)
	}
	if reason == "" {
		reason = fmt.Sprintf("%s reported a material disagreement", conflict.RaisedBy)
	}
	if conflict.Wave > 0 {
		return fmt.Sprintf("after wave %d — %s", conflict.Wave, reason)
	}
	return reason
}

func (m *Model) showSettings() {
	effective := m.orchestrator.EffectiveSettings()
	defaults := m.orchestrator.DefaultSettings()
	roomState, _ := m.orchestrator.Snapshot()
	details := "off"
	if m.showDetails {
		details = "on"
	}
	lines := []string{"Agent settings (effective; personal default):", "Behind-the-scenes details: " + details + " (/details [on|off])"}
	for _, participant := range m.activityParticipants() {
		scope := "inherits default"
		if _, ok := roomState.Settings[participant]; ok {
			scope = "room override"
		}
		lines = append(lines, fmt.Sprintf("%-7s %s (%s)\n        default: %s", strings.ToUpper(string(participant)), settingsSummary(effective[participant]), scope, settingsSummary(defaults[participant])))
	}
	lines = append(lines,
		"Set room: /model @codex MODEL | /effort @agy LEVEL | /permissions @all PROFILE",
		"Set personal default: /model default @codex MODEL (same form for effort/permissions)",
		"Remove room override: /inherit @agent|@all",
		"Models accept provider aliases or full IDs. Use default/auto to clear model/effort overrides.")
	m.addNotice(strings.Join(lines, "\n"))
}

func (m *Model) showAgents() {
	roomState, _ := m.orchestrator.Snapshot()
	available := make(map[chat.Participant]bool)
	for _, participant := range m.orchestrator.Participants() {
		available[participant] = true
	}
	lines := []string{"Room roster:"}
	for _, participant := range chat.Agents() {
		state := "unavailable (CLI not found)"
		if available[participant] {
			state = "away"
			if roomState.Present(participant) {
				state = "present"
			}
		}
		lines = append(lines, fmt.Sprintf("%-8s %s", strings.ToUpper(string(participant)), state))
	}
	lines = append(lines, "Use /join @agent or /leave @agent. Returning agents retain their saved session and catch up on missed room messages.")
	m.addNotice(strings.Join(lines, "\n"))
}

func (m Model) currentSettings() map[chat.Participant]chat.AgentSettings {
	if m.orchestrator != nil {
		return m.orchestrator.EffectiveSettings()
	}
	return map[chat.Participant]chat.AgentSettings{
		chat.Codex:   {Permissions: chat.PermissionWorkspace},
		chat.Claude:  {Permissions: chat.PermissionWorkspace},
		chat.Agy:     {Permissions: chat.PermissionWorkspace},
		chat.Copilot: {Permissions: chat.PermissionWorkspace},
	}
}

func loadModels(orchestrator *room.Orchestrator, participant chat.Participant) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := orchestrator.Models(ctx, participant)
		return modelsMsg{participant: participant, models: models, err: err}
	}
}

func formatModels(participant chat.Participant, models []agent.ModelOption) string {
	lines := []string{strings.ToUpper(string(participant)) + " models:"}
	for _, model := range models {
		label := model.ID
		if model.Default {
			label += " (default)"
		}
		if model.Name != "" && model.Name != model.ID {
			label += " — " + model.Name
		}
		if len(model.Efforts) > 0 {
			label += " [effort: " + strings.Join(model.Efforts, ", ") + "]"
		}
		lines = append(lines, label)
	}
	if participant == chat.Claude {
		lines = append(lines, "Full provider model IDs are also accepted.")
	}
	return strings.Join(lines, "\n")
}

func parseSettingsChange(command string, fields []string) (settingsChange, error) {
	change := settingsChange{Field: strings.TrimPrefix(command, "/")}
	index := 1
	if len(fields) > index && strings.EqualFold(fields[index], "default") {
		change.Default = true
		index++
	}
	participants, err := parseSettingsParticipants(fields, index)
	if err != nil {
		return settingsChange{}, fmt.Errorf("usage: %s [default] @agent|@all VALUE", command)
	}
	index++
	if len(fields) <= index {
		return settingsChange{}, fmt.Errorf("usage: %s [default] @agent|@all VALUE", command)
	}
	change.Participants = participants
	change.Value = strings.TrimSpace(strings.Join(fields[index:], " "))
	if change.Field == "permissions" {
		change.Value = strings.ToLower(change.Value)
		if !chat.PermissionProfile(change.Value).Valid() {
			return settingsChange{}, fmt.Errorf("permissions must be read-only, workspace, or full")
		}
	}
	if change.Field == "effort" {
		change.Value = strings.ToLower(change.Value)
		candidate := chat.AgentSettings{Effort: change.Value, Permissions: chat.PermissionWorkspace}
		if change.Value == "default" {
			candidate.Effort = ""
		}
		if err := appsettings.Validate(candidate); err != nil {
			return settingsChange{}, err
		}
	}
	return change, nil
}

func parseSettingsParticipants(fields []string, index int) ([]chat.Participant, error) {
	if len(fields) <= index {
		return nil, fmt.Errorf("missing agent")
	}
	value := strings.ToLower(fields[index])
	if value == "@all" {
		return chat.Agents(), nil
	}
	participant, ok := chat.ParseParticipant(strings.TrimPrefix(value, "@"))
	if !ok || !strings.HasPrefix(value, "@") {
		return nil, fmt.Errorf("invalid agent")
	}
	return []chat.Participant{participant}, nil
}

func (m *Model) applySettingsChange(change settingsChange) error {
	current := m.orchestrator.RoomSettings()
	if change.Default {
		current = m.orchestrator.DefaultSettings()
	}
	updates := make(map[chat.Participant]chat.AgentSettings, len(change.Participants))
	for _, participant := range change.Participants {
		value := current[participant].WithDefaults()
		switch change.Field {
		case "model":
			value.Model = change.Value
			if strings.EqualFold(value.Model, "default") {
				value.Model = ""
			}
		case "effort":
			value.Effort = change.Value
			if value.Effort == "default" || value.Effort == "auto" {
				value.Effort = ""
			}
		case "permissions":
			value.Permissions = chat.PermissionProfile(change.Value)
		default:
			return fmt.Errorf("unknown settings field %q", change.Field)
		}
		value = appsettings.Normalize(value)
		if err := appsettings.ValidateFor(participant, value); err != nil {
			return err
		}
		updates[participant] = value
	}
	for _, participant := range change.Participants {
		value := updates[participant]
		if err := m.orchestrator.SetAgentSettings(participant, value, change.Default); err != nil {
			return err
		}
	}
	m.addNotice("Updated " + change.Field + " for " + settingsParticipantsLabel(change.Participants))
	return nil
}

func (m *Model) syncRoom() {
	m.room, m.messages = m.orchestrator.Snapshot()
	m.syncRosterActivity()
	m.refreshContent()
}

func (m *Model) syncRoomMetadata() {
	m.room, _ = m.orchestrator.Snapshot()
	m.syncRosterActivity()
	m.refreshContent()
}

func (m *Model) syncRosterActivity() {
	m.ensureActivityMap()
	for _, participant := range m.activityParticipants() {
		if !m.room.Present(participant) {
			m.activity[participant] = participantActivity{Phase: phaseAway, Detail: "not participating"}
		} else if m.activity[participant].Phase == phaseAway || m.activity[participant].Phase == "" {
			m.activity[participant] = participantActivity{Phase: phaseIdle, Detail: "in room"}
		}
	}
}

func settingsParticipantsLabel(participants []chat.Participant) string {
	if len(participants) == len(chat.Agents()) {
		return "all agents"
	}
	if len(participants) == 1 {
		return string(participants[0])
	}
	return "agents"
}

func settingsSummary(value chat.AgentSettings) string {
	model := value.Model
	if model == "" {
		model = "provider default"
	}
	effort := value.Effort
	if effort == "" {
		effort = "auto effort"
	}
	return fmt.Sprintf("%s, %s, %s", model, effort, value.WithDefaults().Permissions)
}

func compactSettings(value chat.AgentSettings) string {
	model := value.Model
	if model == "" {
		model = "default"
	}
	if len([]rune(model)) > 18 {
		model = string([]rune(model)[:17]) + "…"
	}
	effort := value.Effort
	if effort == "" {
		effort = "auto"
	}
	return model + " · " + effort + " · " + string(value.WithDefaults().Permissions)
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
	case chat.Agy:
		return agyStyle
	case chat.Copilot:
		return copilotStyle
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
