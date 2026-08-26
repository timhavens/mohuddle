package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/remote/device"
	"github.com/timhavens/mohuddle/internal/room"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/speech"
	"github.com/timhavens/mohuddle/internal/store"
)

type RoomLister interface {
	ListRooms() ([]chat.Room, error)
}

type RemoteDeviceStore interface {
	CreateInvitation(string, string, []device.Scope, time.Duration) (device.Invitation, error)
	List() []device.Grant
	Revoke(string) error
	SetScopes(string, []device.Scope) (device.Grant, error)
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
	phaseReading    activityPhase = "reading"
	phasePlanning   activityPhase = "planning"
	phaseEditing    activityPhase = "editing"
	phaseTesting    activityPhase = "testing"
	phaseWaiting    activityPhase = "waiting"
	phaseQuiet      activityPhase = "quiet"
	phaseAttention  activityPhase = "needs attention"
	phaseBlocked    activityPhase = "blocked"
	phaseApproval   activityPhase = "needs approval"
	phaseError      activityPhase = "error"
	phaseAway       activityPhase = "away"
)

type participantActivity struct {
	Phase     activityPhase
	Detail    string
	Role      string
	Task      string
	StartedAt time.Time
	UpdatedAt time.Time
}

type settingsChange struct {
	Field        string
	Participants []chat.Participant
	Value        string
	Default      bool
}

type Model struct {
	orchestrator             *room.Orchestrator
	lister                   RoomLister
	composerStore            composerStore
	room                     chat.Room
	messages                 []chat.Message
	input                    textarea.Model
	pastes                   []string
	attachments              []chat.Attachment
	history                  []chat.ComposerHistoryEntry
	historyIndex             int
	historyDraft             *chat.ComposerHistoryEntry
	clipboard                clipboardReader
	clipboardBusy            bool
	suggestionIndex          int
	suggestionsHidden        bool
	viewport                 viewport.Model
	following                bool
	unseen                   int
	width                    int
	height                   int
	ready                    bool
	mouseCaptured            bool
	status                   string
	notices                  []noticeEntry
	live                     map[chat.Participant]string
	liveTurnIDs              map[chat.Participant]string
	liveStates               map[chat.Participant]chat.TurnRecordState
	streamMode               chat.StreamMode
	turns                    []chat.TurnRecord
	turnDetailsOpen          bool
	turnIndex                int
	turnViewport             viewport.Model
	activity                 map[chat.Participant]participantActivity
	now                      time.Time
	spinnerFrame             int
	pending                  *agent.ApprovalRequest
	approvalQueue            []*agent.ApprovalRequest
	showDetails              bool
	progressMode             chat.ProgressMode
	completionSound          bool
	completionNotifier       completionNotifier
	completionSoundError     bool
	remoteDevices            RemoteDeviceStore
	remoteOrigin             string
	remoteAudit              *api.AuditLog
	speech                   speech.Controller
	speechState              speech.State
	fullConfirmation         *settingsChange
	planChoice               int
	delegationChoice         int
	routeChoice              int
	routeReplaceConfirm      bool
	conversationReplace      string
	roomDeleteConfirm        string
	preserveTranscriptOffset bool
	repliesOpen              bool
	replyIndex               int
	replyViewport            viewport.Model
	action                   ExitAction
	quitting                 bool
}

type roomEventMsg struct {
	event room.Event
	open  bool
}

type activityTickMsg time.Time

type speechEventMsg struct {
	event speech.Event
	open  bool
}

type modelsMsg struct {
	participant chat.Participant
	models      []agent.ModelOption
	err         error
}

type voicesMsg struct {
	filter string
	voices []speech.Voice
	err    error
}

var (
	headerStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Padding(0, 1)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	codexStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	claudeStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	agyStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	copilotStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("141"))
	systemStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("150"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	busyStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	waitStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	moderatorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62"))
	planStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214"))
	modalStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2)
	composerColor  = lipgloss.Color("235")
	composerStyle  = lipgloss.NewStyle().Background(composerColor).Padding(1, 1)
)

var activitySpinner = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func New(orchestrator *room.Orchestrator, lister RoomLister, controllers ...speech.Controller) Model {
	roomState, messages := orchestrator.Snapshot()
	input := newComposerInput()
	now := time.Now()
	model := Model{
		orchestrator:       orchestrator,
		lister:             lister,
		room:               roomState,
		messages:           messages,
		input:              input,
		viewport:           viewport.New(80, 20),
		replyViewport:      viewport.New(76, 14),
		turnViewport:       viewport.New(76, 14),
		following:          true,
		mouseCaptured:      true,
		status:             "ready",
		clipboard:          systemClipboard{},
		live:               map[chat.Participant]string{},
		liveTurnIDs:        map[chat.Participant]string{},
		liveStates:         map[chat.Participant]chat.TurnRecordState{},
		streamMode:         roomState.StreamMode.WithDefault(),
		turns:              cloneTurnRecords(roomState.TurnHistory),
		activity:           map[chat.Participant]participantActivity{},
		showDetails:        orchestrator.DetailsVisible(),
		progressMode:       orchestrator.ProgressDisplayMode(),
		completionSound:    orchestrator.CompletionSoundEnabled(),
		completionNotifier: defaultCompletionNotifier(),
		now:                now,
	}
	if store, ok := lister.(composerStore); ok {
		model.composerStore = store
		history, err := store.LoadComposerHistory(roomState.ID)
		if err != nil {
			model.notices = append(model.notices, noticeEntry{Text: errorStyle.Render("Could not load input history: " + err.Error()), CreatedAt: time.Now()})
		} else {
			model.history = history
		}
	}
	model.historyIndex = len(model.history)
	if len(controllers) > 0 && controllers[0] != nil {
		model.speech = controllers[0]
		model.speechState = controllers[0].Snapshot()
	}
	for _, participant := range orchestrator.Participants() {
		phase := phaseAway
		if roomState.Present(participant) {
			phase = phaseIdle
		}
		model.activity[participant] = participantActivity{Phase: phase}
		if persisted, ok := roomState.Activities[participant]; ok {
			model.activity[participant] = participantActivityFromStructured(persisted, now)
		}
	}
	if roomState.Conflict != nil {
		model.status = "conflict requires your direction"
	} else if roomState.PendingDelegation != nil {
		model.status = "delegation split proposed; choose an action below"
	} else if roomState.PendingPlan != nil {
		model.status = "plan ready; choose an action below"
	} else if len(roomState.PendingRoutes) > 0 {
		model.status = "a message needs routing"
	}
	return model
}

// ConfigureRemote exposes only trusted local device-management controls to the
// TUI. The browser gateway itself never receives this store or audit authority.
func (m *Model) ConfigureRemote(devices RemoteDeviceStore, origin string, audit *api.AuditLog) {
	m.remoteDevices = devices
	m.remoteOrigin = strings.TrimSuffix(strings.TrimSpace(origin), "/")
	m.remoteAudit = audit
}

// ConfigureStartupNotice adds host diagnostics to the visible room timeline
// before Bubble Tea enters its alternate screen. It is intentionally local UI
// state: startup guidance must remain visible without mutating room history.
func (m *Model) ConfigureStartupNotice(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	m.notices = append(m.notices, noticeEntry{Text: text, CreatedAt: time.Now()})
	m.status = "setup guidance available"
}

func newComposerInput() textarea.Model {
	input := textarea.New()
	input.Placeholder = "Message the room, target @agent, or /delegate to a room AI…"
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.CharLimit = 32 * 1024
	input.SetHeight(1)
	input.SetWidth(input.Width())

	styleComposer := func(style textarea.Style) textarea.Style {
		style.Base = style.Base.Background(composerColor)
		style.CursorLine = style.CursorLine.Background(composerColor)
		style.CursorLineNumber = style.CursorLineNumber.Background(composerColor)
		style.EndOfBuffer = style.EndOfBuffer.Background(composerColor)
		style.LineNumber = style.LineNumber.Background(composerColor)
		style.Placeholder = style.Placeholder.Background(composerColor)
		style.Prompt = style.Prompt.Bold(true).Foreground(lipgloss.Color("252")).Background(composerColor)
		style.Text = style.Text.Background(composerColor)
		return style
	}
	input.FocusedStyle = styleComposer(input.FocusedStyle)
	input.BlurredStyle = styleComposer(input.BlurredStyle)
	input.Focus()
	return input
}

func (m Model) Init() tea.Cmd {
	commands := []tea.Cmd{textarea.Blink, waitForRoomEvent(m.orchestrator.Events()), activityTick()}
	if m.speech != nil {
		commands = append(commands, waitForSpeechEvent(m.speech.Events()))
	}
	return tea.Batch(commands...)
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

func waitForSpeechEvent(events <-chan speech.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return speechEventMsg{event: event, open: ok}
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
	case speechEventMsg:
		if value.open {
			m.applySpeechEvent(value.event)
			if m.speech != nil {
				commands = append(commands, waitForSpeechEvent(m.speech.Events()))
			}
		}
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
	case voicesMsg:
		if value.err != nil {
			m.addNotice(errorStyle.Render(value.err.Error()))
			m.status = "voice catalog failed"
		} else {
			m.addNotice(formatVoices(value.filter, value.voices))
			m.status = "voice catalog loaded"
		}
	case clipboardMsg:
		m.clipboardBusy = false
		content := clipboardContent(value)
		switch {
		case content.Err != nil:
			m.addNotice(errorStyle.Render("Paste failed: " + content.Err.Error()))
			m.status = "paste failed"
		case len(content.Image) > 0:
			if err := m.acceptClipboardImage(content.Image); err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				m.status = "image paste failed"
			} else {
				m.status = "image attached"
			}
		case compactPastedText(content.Text):
			m.addPastedText(content.Text)
			m.status = "pasted content attached"
		default:
			m.input.SetValue(m.input.Value() + content.Text)
			m.input.CursorEnd()
			m.resize()
		}
	case tea.MouseMsg:
		if m.ready {
			var command tea.Cmd
			m.viewport, command = m.viewport.Update(value)
			commands = append(commands, command)
			m.following = m.viewport.AtBottom()
			if m.following {
				m.unseen = 0
			}
		}
		return m, tea.Batch(commands...)
	case tea.KeyMsg:
		switch strings.ToLower(value.String()) {
		case "alt+m":
			m.mouseCaptured = !m.mouseCaptured
			if m.mouseCaptured {
				m.status = "mouse scroll enabled"
				return m, tea.EnableMouseCellMotion
			}
			m.status = "text selection enabled"
			return m, tea.DisableMouse
		case "alt+v":
			m.toggleSpeech()
			return m, tea.Batch(commands...)
		}
		if m.pending != nil {
			if m.handleApprovalKey(value) {
				m.refreshContent()
				return m, tea.Batch(commands...)
			}
			return m, tea.Batch(commands...)
		}
		if m.room.PendingDelegation != nil {
			if m.handleDelegationDecisionKey(value) {
				m.resize()
				return m, tea.Batch(commands...)
			}
			return m, tea.Batch(commands...)
		}
		if m.room.PendingPlan != nil {
			if m.handlePlanDecisionKey(value) {
				m.resize()
				return m, tea.Batch(commands...)
			}
			return m, tea.Batch(commands...)
		}
		if len(m.room.PendingRoutes) > 0 {
			if m.handleRouteDecisionKey(value) {
				m.resize()
				return m, tea.Batch(commands...)
			}
			return m, tea.Batch(commands...)
		}
		if m.handleTurnDetailsShortcut(value) {
			m.resize()
			return m, tea.Batch(commands...)
		}
		if m.handleConversationShortcut(value) {
			m.resize()
			return m, tea.Batch(commands...)
		}
		if value.String() == "shift+tab" {
			if m.fullConfirmation == nil {
				m.toggleWorkflowMode()
			}
			return m, tea.Batch(commands...)
		}
		if value.Paste {
			pasted := string(value.Runes)
			if compactPastedText(pasted) {
				m.addPastedText(pasted)
				m.status = "pasted content attached"
				return m, tea.Batch(commands...)
			}
		}
		if m.handleTranscriptKey(value.String()) {
			return m, tea.Batch(commands...)
		}
		switch value.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "ctrl+v":
			if !m.clipboardBusy && m.clipboard != nil {
				m.clipboardBusy = true
				m.status = "reading clipboard"
				commands = append(commands, readClipboard(m.clipboard))
			}
			return m, tea.Batch(commands...)
		case "ctrl+p":
			m.browseHistory(-1)
			return m, tea.Batch(commands...)
		case "ctrl+n":
			m.browseHistory(1)
			return m, tea.Batch(commands...)
		case "up", "down":
			if matches := m.matchingCommands(); len(matches) > 0 {
				if value.String() == "up" {
					m.suggestionIndex = (m.suggestionIndex - 1 + len(matches)) % len(matches)
				} else {
					m.suggestionIndex = (m.suggestionIndex + 1) % len(matches)
				}
				return m, tea.Batch(commands...)
			}
			if !strings.Contains(m.input.Value(), "\n") {
				delta := -1
				if value.String() == "down" {
					delta = 1
				}
				if m.browseHistory(delta) {
					return m, tea.Batch(commands...)
				}
			}
		case "tab":
			if m.completeSuggestion() {
				return m, tea.Batch(commands...)
			}
		case "backspace":
			if m.removeLastComposerItem() {
				return m, tea.Batch(commands...)
			}
		case "alt+enter":
			m.input.InsertString("\n")
			m.resize()
			return m, tea.Batch(commands...)
		case "esc":
			if len(m.matchingCommands()) > 0 {
				m.suggestionsHidden = true
				m.resize()
				return m, tea.Batch(commands...)
			}
			if m.orchestrator.HasActiveWork() || m.orchestrator.PendingInputCount() > 0 {
				m.orchestrator.Stop()
				m.stopActivities()
				m.status = "stopping active and queued work"
			}
			return m, tea.Batch(commands...)
		case "ctrl+enter":
			text := m.composedText()
			if strings.TrimSpace(text) == "" && len(m.attachments) == 0 {
				return m, tea.Batch(commands...)
			}
			entry := m.currentComposerEntry()
			attachments := append([]chat.Attachment(nil), m.attachments...)
			m.addHistory(entry)
			m.resetComposer()
			m.following = true
			m.viewport.GotoBottom()
			if command := m.submit("/steer "+text, attachments); command != nil {
				return m, command
			}
			return m, nil
		case "enter":
			if !value.Alt {
				text := m.composedText()
				if strings.TrimSpace(text) != "" || len(m.attachments) > 0 {
					if len(m.attachments) > 0 && !supportsConversationAttachments(m.input.Value()) {
						m.addNotice(errorStyle.Render("Images can be sent with a message, /ask, or /round; remove the image before running this command"))
						return m, tea.Batch(commands...)
					}
					entry := m.currentComposerEntry()
					attachments := append([]chat.Attachment(nil), m.attachments...)
					m.addHistory(entry)
					m.resetComposer()
					m.following = true
					m.unseen = 0
					m.viewport.GotoBottom()
					if command := m.submit(text, attachments); command != nil {
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
	if _, ok := message.(tea.KeyMsg); ok {
		m.suggestionsHidden = false
		m.historyIndex = len(m.history)
		m.historyDraft = nil
		m.resize()
	}
	return m, tea.Batch(commands...)
}

func (m *Model) submit(value string, attachmentGroups ...[]chat.Attachment) tea.Cmd {
	var attachments []chat.Attachment
	if len(attachmentGroups) > 0 {
		attachments = attachmentGroups[0]
	}
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
		if err := m.orchestrator.PostWithAttachments(value, attachments); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoomMetadata()
			switch {
			case len(m.room.PendingRoutes) > 0:
				m.status = "message needs routing"
			case m.orchestrator.PendingInputCount() > 0:
				m.status = fmt.Sprintf("work queued (%d pending)", m.orchestrator.PendingInputCount())
			default:
				m.status = "message accepted"
			}
		}
		return nil
	}
	fields := strings.Fields(value)
	command := strings.ToLower(fields[0])
	switch command {
	case "/plan":
		mode := m.room.WorkflowMode.WithDefault()
		switch {
		case len(fields) == 1:
			if mode.PlanOnly() {
				mode = chat.WorkflowExecute
			} else {
				mode = chat.WorkflowPlan
			}
		case len(fields) == 2 && strings.EqualFold(fields[1], "status"):
			label := "Default"
			if mode.PlanOnly() {
				label = "Plan"
			}
			m.addNotice("Workflow mode is " + label + ". Shift+Tab toggles Default and Plan without interrupting active work.")
			return nil
		case len(fields) == 2 && strings.EqualFold(fields[1], "on"):
			mode = chat.WorkflowPlan
		case len(fields) == 2 && strings.EqualFold(fields[1], "off"):
			mode = chat.WorkflowExecute
		default:
			m.addNotice(errorStyle.Render("usage: /plan [on|off|status]"))
			return nil
		}
		m.setWorkflowMode(mode)
	case "/delegation":
		policy := m.orchestrator.DelegationPolicy()
		if len(fields) == 1 || (len(fields) == 2 && strings.EqualFold(fields[1], "status")) {
			m.addNotice("Delegation policy is " + string(policy) + ". Adaptive runs useful read-only splits automatically in Plan mode, asks in Execute mode, and preserves explicit /ask, /round, and @agent topology.")
			break
		}
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /delegation [adaptive|auto|ask|manual|status]"))
			break
		}
		policy = chat.DelegationPolicy(strings.ToLower(fields[1]))
		if err := m.orchestrator.SetDelegationPolicy(policy); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.syncRoomMetadata()
		m.status = "delegation policy set to " + string(policy)
	case "/parallel", "/solo":
		prompt := strings.TrimSpace(value[len(fields[0]):])
		if prompt == "" && len(attachments) == 0 {
			m.addNotice(errorStyle.Render("usage: " + command + " MESSAGE"))
			break
		}
		var err error
		if command == "/parallel" {
			err = m.orchestrator.PostParallelWithAttachments(prompt, attachments)
		} else {
			err = m.orchestrator.PostSoloWithAttachments(prompt, attachments)
		}
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoomMetadata()
			m.status = strings.TrimPrefix(command, "/") + " request accepted"
		}
	case "/search":
		enabled := m.orchestrator.WebSearchEnabled()
		switch {
		case len(fields) == 1 || (len(fields) == 2 && strings.EqualFold(fields[1], "status")):
			state := "off"
			if enabled {
				state = "on"
			}
			m.addNotice("Host-mediated public web research is " + state + ". Use /search on or /search off; this setting is independent of Default/Plan mode.")
			return nil
		case len(fields) == 2 && strings.EqualFold(fields[1], "on"):
			enabled = true
		case len(fields) == 2 && strings.EqualFold(fields[1], "off"):
			enabled = false
		default:
			m.addNotice(errorStyle.Render("usage: /search [on|off|status]"))
			return nil
		}
		if err := m.orchestrator.SetWebSearchEnabled(enabled); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		state := "off"
		if enabled {
			state = "on"
		}
		m.status = "web research " + state
		m.addNotice("Host-mediated public web research is now " + state + " in both Default and Plan modes.")
	case "/replies":
		limit := m.orchestrator.ConversationResponderLimit()
		if len(fields) == 2 && strings.EqualFold(fields[1], "dismiss-all") {
			if err := m.orchestrator.DismissAllConversations(); err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
			m.syncRoom()
			m.status = "visible replies dismissed"
			break
		}
		if len(fields) == 1 || (len(fields) == 2 && strings.EqualFold(fields[1], "status")) {
			m.addNotice(fmt.Sprintf("Temporary chat responders: %d (allowed 0–8; main work keeps priority).", limit))
			break
		}
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /replies [0-8|status|dismiss-all]"))
			break
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil {
			m.addNotice(errorStyle.Render("usage: /replies [0-8|status|dismiss-all]"))
			break
		}
		if err := m.orchestrator.SetConversationResponderLimit(value); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.status = fmt.Sprintf("temporary chat responders set to %d", value)
		m.addNotice("Temporary chat responder limit saved. Main-work provider capacity remains reserved.")
	case "/steer":
		prompt := strings.TrimSpace(value[len(fields[0]):])
		if prompt == "" && len(attachments) == 0 {
			m.addNotice(errorStyle.Render("usage: /steer MESSAGE"))
			break
		}
		if err := m.orchestrator.SteerWithAttachments(prompt, attachments); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.status = "active work replaced by explicit steering"
		}
	case "/ask", "/once":
		prompt := strings.TrimSpace(value[len(fields[0]):])
		if prompt == "" {
			m.addNotice(errorStyle.Render("usage: " + command + " [@agent ...] MESSAGE"))
			break
		}
		if err := m.orchestrator.AskWithAttachments(prompt, attachments); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.status = "one-shot question sent"
		}
	case "/round":
		prompt := strings.TrimSpace(value[len(fields[0]):])
		if prompt == "" {
			m.addNotice(errorStyle.Render("usage: /round [@agent ...] MESSAGE"))
			break
		}
		if err := m.orchestrator.RoundWithAttachments(prompt, attachments); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.status = "read-only group round sent"
		}
	case "/continue":
		if err := m.orchestrator.Continue(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoom()
			m.status = "round continuing"
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
	case "/progress":
		if len(fields) == 1 {
			m.addNotice("Progress display is " + string(m.progressMode.WithDefault()) + ". Use /progress compact, /progress detailed, or /progress off.")
			break
		}
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /progress [compact|detailed|off]"))
			break
		}
		mode := chat.ProgressMode(strings.ToLower(fields[1]))
		if err := m.orchestrator.SetProgressDisplayMode(mode); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.progressMode = mode
		m.status = "progress display " + string(mode)
		m.resize()
	case "/stream":
		if len(fields) == 1 {
			m.addNotice("Response streaming is " + string(m.streamMode.WithDefault()) + ". Use /stream stable, /stream live, or /stream history.")
			break
		}
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /stream [stable|live|history]"))
			break
		}
		mode := chat.StreamMode(strings.ToLower(fields[1]))
		if err := m.orchestrator.SetStreamMode(mode); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.streamMode = mode
		if mode == chat.StreamStable {
			m.live = map[chat.Participant]string{}
			m.liveTurnIDs = map[chat.Participant]string{}
			m.liveStates = map[chat.Participant]chat.TurnRecordState{}
			m.turnDetailsOpen = false
		} else if mode == chat.StreamHistory {
			roomState, _ := m.orchestrator.Snapshot()
			m.room = roomState
			m.turns = cloneTurnRecords(roomState.TurnHistory)
			m.turnIndex = len(m.turns) - 1
		}
		m.status = "response streaming " + string(mode)
		if mode == chat.StreamHistory {
			m.addNotice("Turn history is enabled. Press Alt+T to inspect retained response drafts and tool activity.")
		}
		m.resize()
	case "/sound":
		enabled, err := parseToggle(fields, m.completionSound, "/sound")
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		if err := m.orchestrator.SetCompletionSoundEnabled(enabled); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.completionSound = enabled
		m.completionSoundError = false
		state := "off"
		if enabled {
			state = "on"
		}
		m.status = "AI-finished sound " + state
		m.addNotice("AI-finished terminal sound is now " + state + ".")
	case "/speak":
		m.handleSpeak(fields)
	case "/voice":
		m.handleVoice(fields)
	case "/voices":
		if m.speech == nil {
			m.addNotice(errorStyle.Render("speech service is unavailable"))
			break
		}
		filter := strings.TrimSpace(strings.Join(fields[1:], " "))
		m.status = "loading Edge voice catalog"
		return loadVoices(m.speech, filter)
	case "/status":
		if err := m.orchestrator.RefreshCoreState(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		lines := []string{room.FormatStatusSnapshot(m.orchestrator.StatusSnapshot())}
		if m.remoteDevices != nil {
			active, revoked := remoteDeviceCounts(m.remoteDevices.List())
			lines = append(lines, fmt.Sprintf("remote phone gateway: %s; devices active %d, revoked %d; ceiling read-only", m.remoteOrigin, active, revoked))
		} else {
			lines = append(lines, "remote phone gateway: disabled")
		}
		configured := m.orchestrator.EffectiveSettings()
		installed := make(map[chat.Participant]bool)
		for _, participant := range m.orchestrator.Participants() {
			installed[participant] = true
		}
		for _, participant := range configuredRosterParticipants(m.orchestrator.Participants(), m.orchestrator.WorkerCounts()) {
			if installed[participant] {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: unavailable (provider CLI/runtime); %s", strings.ToUpper(string(participant)), settingsSummary(configured[participant])))
		}
		m.addNotice(strings.Join(lines, "\n"))
	case "/bump":
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /bump @agent"))
			break
		}
		participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(fields[1], "@")))
		if !ok || !strings.HasPrefix(fields[1], "@") {
			m.addNotice(errorStyle.Render("usage: /bump @agent"))
			break
		}
		result, err := m.orchestrator.Bump(participant)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.addNotice(result.String())
	case "/agents":
		m.showAgents()
	case "/workers":
		if len(fields) == 1 || (len(fields) == 2 && strings.EqualFold(fields[1], "show")) {
			m.showWorkers()
			break
		}
		currentCounts := m.orchestrator.WorkerCounts()
		counts, err := parseWorkerCounts(fields, currentCounts)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		if workerCountsEqual(currentCounts, counts) {
			m.addNotice("Auxiliary worker topology is unchanged")
			break
		}
		if m.orchestrator.HasActiveWork() {
			m.addNotice(errorStyle.Render("worker topology cannot change while agent work is active; use /stop or wait for completion"))
			break
		}
		if err := m.orchestrator.SetWorkerCounts(counts); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.action.ResumeID = m.room.ID
		m.quitting = true
		return tea.Quit
	case "/delegate":
		participant, task, err := parseDelegation(value)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		if err := m.orchestrator.Delegate(participant, task); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.status = fmt.Sprintf("subtask delegated to %s", participant)
		}
	case "/core":
		m.handleCore(fields)
	case "/roster":
		m.handleRoster(fields, time.Now())
	case "/remote":
		m.handleRemote(fields, value)
	case "/moderator":
		if len(fields) == 1 {
			moderator := m.orchestrator.Moderator()
			if moderator == "" {
				m.addNotice("No moderator is available; join or promote an available core peer.")
			} else {
				m.addNotice(strings.ToUpper(string(moderator)) + " is the room moderator.")
			}
			break
		}
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /moderator @agent|auto"))
			break
		}
		if strings.EqualFold(fields[1], "auto") {
			if err := m.orchestrator.SetModeratorAutomatic(); err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
			m.syncRoomMetadata()
			m.status = "moderator selection is automatic"
			break
		}
		participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(fields[1], "@")))
		if !ok || !strings.HasPrefix(fields[1], "@") {
			m.addNotice(errorStyle.Render("usage: /moderator @agent|auto"))
			break
		}
		if err := m.orchestrator.SetModerator(participant); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		} else {
			m.syncRoomMetadata()
			m.status = string(participant) + " is moderating"
		}
	case "/join", "/leave":
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: " + command + " @agent|@all"))
			break
		}
		participants, err := parseSettingsParticipants(fields, 1)
		if err != nil {
			m.addNotice(errorStyle.Render("usage: " + command + " @agent|@all"))
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
		if coreStatus := m.orchestrator.CoreStatus(); coreStatus.Policy.Failover == chat.CoreFailoverPrompt && len(coreStatus.Active) < len(coreStatus.Policy.Preferred) {
			m.showCoreStatus()
		}
	case "/settings":
		m.showSettings()
	case "/models":
		participants, err := parseSettingsParticipants(fields, 1)
		if err != nil || len(participants) != 1 {
			m.addNotice(errorStyle.Render("usage: /models @agent"))
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
		if settingsTargetAll(fields) {
			change.Participants = m.configuredParticipants()
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
		if strings.EqualFold(fields[1], "@all") {
			participants = m.configuredParticipants()
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
		if len(fields) >= 2 && strings.EqualFold(fields[1], "delete") {
			if len(fields) != 3 && !(len(fields) == 4 && strings.EqualFold(fields[3], "confirm")) {
				m.addNotice(errorStyle.Render("usage: /rooms delete ID [confirm]"))
				break
			}
			id := fields[2]
			if id == m.room.ID {
				m.addNotice(errorStyle.Render("cannot delete the room currently open in this instance; use /quit first"))
				break
			}
			var selected *chat.Room
			for index := range rooms {
				if rooms[index].ID == id {
					selected = &rooms[index]
					break
				}
			}
			if selected == nil {
				m.addNotice(errorStyle.Render("saved room not found"))
				break
			}
			manager, ok := m.lister.(interface {
				RoomMessageCount(string) (int, error)
				DeleteRoom(string) (store.RoomDeleteInfo, error)
			})
			if !ok {
				m.addNotice(errorStyle.Render("room deletion is unavailable"))
				break
			}
			count, err := manager.RoomMessageCount(id)
			if err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
			if len(fields) != 4 || m.roomDeleteConfirm != id {
				m.roomDeleteConfirm = id
				m.addNotice(fmt.Sprintf("Delete room %s?\nworkspace: %s\nmessages: %d\nRun /rooms delete %s confirm to delete it.", id, selected.Workspace, count, id))
				break
			}
			info, err := manager.DeleteRoom(id)
			m.roomDeleteConfirm = ""
			if err != nil {
				m.addNotice(errorStyle.Render(err.Error()))
				break
			}
			m.status = "saved room deleted"
			m.addNotice(fmt.Sprintf("Deleted room %s (%s, %d messages).", info.ID, info.Workspace, info.MessageCount))
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
		m.addNotice("Commands include /status, /stream stable|live|history, /delegation adaptive|auto|ask|manual, /parallel MESSAGE, /solo MESSAGE, /delegate @agent TASK, /bump @agent, /rooms, /rooms delete ID, /new, /resume ID, /stop, /help, plus the workflow, roster, provider, settings, access, remote, speech, and research controls shown by completion.\nAlt+T opens retained Turn details in history mode. Alt+S opens the Replies inbox. In that panel, Up/Down navigates replies, PageUp/PageDown scrolls, Esc closes, Alt+C cancels active replies, Alt+D dismisses answers or decisions, and Alt+W/Alt+R add or replace work when a reply requires a work decision. /replies dismiss-all dismisses every visible non-working card.\nShift+Tab toggles Default and Plan modes for future submissions. Ctrl+Enter explicitly steers and replaces active work; /stop cancels active and queued work.")
	case "/quit", "/exit":
		m.quitting = true
		return tea.Quit
	default:
		m.addNotice("unknown command; use /help")
	}
	return nil
}

func (m *Model) toggleWorkflowMode() {
	mode := m.room.WorkflowMode.WithDefault()
	if mode.PlanOnly() {
		mode = chat.WorkflowExecute
	} else {
		mode = chat.WorkflowPlan
	}
	m.setWorkflowMode(mode)
}

func (m *Model) setWorkflowMode(mode chat.WorkflowMode) {
	if m.orchestrator == nil {
		m.addNotice(errorStyle.Render("workflow mode is unavailable"))
		return
	}
	if err := m.orchestrator.SetWorkflowMode(mode); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.syncRoomMetadata()
	m.suggestionsHidden = true
	if mode.PlanOnly() {
		m.status = "plan mode on; future messages are read-only"
		m.addNotice("Plan mode enabled. Shift+Tab returns to Default mode; switching modes never interrupts active work.")
	} else {
		m.status = "default mode on"
		m.addNotice("Default mode enabled. Existing queued plan messages remain plan-only.")
	}
	m.resize()
}

func correctionStatusLines(messages []chat.Message) []string {
	return correctionStatusLinesFor(messages, chat.Agents())
}

func correctionStatusLinesFor(messages []chat.Message, participants []chat.Participant) []string {
	total, agents := chat.CorrectionStatistics(messages)
	lines := []string{fmt.Sprintf("corrections: offered %d; accepted %d; retracted %d; pending %d", total.Offered, total.Accepted, total.Retracted, total.Pending)}
	for _, participant := range participants {
		counts := agents[participant]
		lines = append(lines, fmt.Sprintf("corrections @%s: offered %d; accepted %d; retracted %d; pending %d; accepted received %d", participant, counts.Offered, counts.Accepted, counts.Retracted, counts.Pending, counts.AcceptedReceived))
	}
	return lines
}

func (m *Model) applyRoomEvent(event room.Event) {
	switch event.Type {
	case room.EventMessage:
		if event.Message != nil {
			if event.Message.ConversationID != "" && event.Message.Author != chat.User && event.Message.Kind == chat.MessageText {
				m.preserveTranscriptOffset = true
			}
			if !m.following {
				m.unseen++
			}
			m.messages = append(m.messages, *event.Message)
			if m.speech != nil && event.Message.Author.ValidAgent() && event.Message.Kind == chat.MessageText {
				text, _ := chat.DisplayProposedPlan(event.Message.Text)
				m.speech.Speak(event.Message.Author, text)
			}
			if event.Message.Author == chat.User && m.room.Conflict != nil {
				roomState, _ := m.orchestrator.Snapshot()
				m.room = roomState
			}
			switch {
			case event.Message.Author == chat.User && event.Message.Kind == chat.MessageText:
				if event.Message.Target.ValidAgent() {
					m.queueActivity(event.Message.Target)
				}
			case event.Message.Author.ValidAgent() && event.Message.Kind == chat.MessageTool:
				m.setActivity(event.Message.Author, activityPhaseForDetail(event.Message.Text), event.Message.Text)
			case event.Message.Author.ValidAgent() && (event.Message.Kind == chat.MessageText || event.Message.Kind == chat.MessageInterrupted):
				detail := "response posted"
				if event.Message.Kind == chat.MessageInterrupted {
					detail = "draft interrupted"
				}
				m.finishActivity(event.Message.Author, detail)
			}
			m.status = fmt.Sprintf("%s posted", event.Message.Author)
		}
	case room.EventRoutingStarted:
		if m.orchestrator != nil {
			m.syncRoomMetadata()
		}
		if strings.TrimSpace(event.Text) != "" {
			m.status = event.Text
		} else {
			m.status = "choosing the core lead"
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
		m.ensureLiveMaps()
		turnID := event.TurnID
		if turnID == "" {
			turnID = "legacy:" + string(event.Participant)
		}
		m.liveTurnIDs[event.Participant] = turnID
		delete(m.live, event.Participant)
		delete(m.liveStates, event.Participant)
		m.setWorkAssignment(event.Participant, event.Role, event.Task)
		if event.WorkflowMode.PlanOnly() {
			m.setActivity(event.Participant, phasePlanning, "planning read-only")
			m.status = fmt.Sprintf("%s is planning", event.Participant)
		} else {
			m.setActivity(event.Participant, phaseThinking, "waiting for model response")
			m.status = fmt.Sprintf("%s is thinking", event.Participant)
		}
	case room.EventTurnFinished:
		m.discardApprovals(event.Participant)
		m.ensureLiveMaps()
		if event.TurnID == "" || m.liveTurnIDs[event.Participant] == "" || m.liveTurnIDs[event.Participant] == event.TurnID {
			if event.Turn == nil {
				delete(m.live, event.Participant)
				delete(m.liveTurnIDs, event.Participant)
				delete(m.liveStates, event.Participant)
				// A contentless failure has no retained response record. Its error and
				// activity events remain authoritative, so do not manufacture a silent
				// review preview here.
			} else {
				if m.streamMode.WithDefault() == chat.StreamHistory || event.Turn.State == chat.TurnRecordInterrupted {
					m.rememberTurn(*event.Turn)
				}
				if m.streamMode.WithDefault() == chat.StreamStable {
					delete(m.live, event.Participant)
					delete(m.liveTurnIDs, event.Participant)
					delete(m.liveStates, event.Participant)
				} else {
					if strings.TrimSpace(publicLiveText(m.live[event.Participant])) == "" && len(event.Turn.Drafts) > 0 {
						m.live[event.Participant] = event.Turn.Drafts[len(event.Turn.Drafts)-1]
					}
					m.liveStates[event.Participant] = event.Turn.State
				}
			}
		}
		if structured, ok := m.room.Activities[event.Participant]; !ok || (structured.State != chat.SchedulerWaiting && structured.State != chat.SchedulerNeedsAttention) {
			m.finishActivity(event.Participant, "")
		}
		m.notifyTurnFinished()
	case room.EventActivity:
		if event.Activity != nil {
			m.activity[event.Activity.Participant] = participantActivityFromStructured(*event.Activity, m.now)
			if m.room.Activities == nil {
				m.room.Activities = make(map[chat.Participant]chat.ParticipantActivity)
			}
			m.room.Activities[event.Activity.Participant] = *event.Activity
		}
	case room.EventAgent:
		if event.AgentEvent == nil {
			break
		}
		agentEvent := event.AgentEvent
		m.ensureLiveMaps()
		participant := agentEvent.Agent
		if participant == "" {
			participant = event.Participant
		}
		if event.TurnID != "" {
			current := m.liveTurnIDs[participant]
			if current != "" && current != event.TurnID {
				break
			}
			m.liveTurnIDs[participant] = event.TurnID
		} else if m.liveTurnIDs[participant] == "" {
			m.liveTurnIDs[participant] = "legacy:" + string(participant)
		}
		switch agentEvent.Type {
		case agent.EventDelta:
			if m.streamMode.WithDefault() != chat.StreamStable {
				m.live[participant] += agentEvent.Text
			}
			m.setActivity(agentEvent.Agent, phaseResponding, "streaming response")
			m.status = fmt.Sprintf("%s is responding", agentEvent.Agent)
		case agent.EventReset:
			delete(m.live, participant)
		case agent.EventTool:
			detail := strings.TrimSpace(agentEvent.Text)
			if detail == "" {
				detail = "tool activity"
			}
			m.setActivity(agentEvent.Agent, activityPhaseForDetail(detail), detail)
			if m.showDetails {
				m.status = fmt.Sprintf("%s used a tool", agentEvent.Agent)
			} else {
				m.status = "agents are working"
			}
		case agent.EventStatus:
			m.setActivity(agentEvent.Agent, activityPhaseForDetail(agentEvent.Text), agentEvent.Text)
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
	case room.EventPlanReady:
		m.syncRoomMetadata()
		m.planChoice = 0
		m.status = "plan ready; choose an action below"
		m.resize()
	case room.EventDelegationAsk:
		m.syncRoomMetadata()
		m.delegationChoice = 0
		m.status = "delegation split proposed; choose Run split or Run solo"
		m.resize()
	case room.EventDelegationDone:
		m.finishActivity(event.Participant, "")
		m.status = event.Text
	case room.EventConflict:
		m.syncRoom()
		m.status = "conflict requires your direction"
	case room.EventQueueChanged:
		m.syncRoomMetadata()
		if event.Queued > 0 {
			m.status = fmt.Sprintf("%d message(s) queued for the next safe boundary", event.Queued)
			if strings.TrimSpace(event.Text) != "" {
				m.status = event.Text
			}
		} else if strings.TrimSpace(event.Text) != "" {
			m.status = event.Text
		}
		m.resize()
	case room.EventConversation:
		m.syncRoom()
		if event.Conversation != nil {
			switch event.Conversation.DerivedInboxCategory() {
			case chat.ConversationInboxNewAnswer:
				m.status = "chat answer ready"
			case chat.ConversationInboxActionNeeded:
				m.status = "a conversation needs attention"
			case chat.ConversationInboxWorking:
				if event.Conversation.State == chat.ConversationWaiting {
					m.status = fmt.Sprintf("conversation waiting in position %d", event.Conversation.QueuePosition)
				} else {
					m.status = "conversation " + string(event.Conversation.State)
				}
			default:
				m.status = "conversation " + string(event.Conversation.State)
			}
		} else if strings.TrimSpace(event.Text) != "" {
			m.status = event.Text
		}
		m.resize()
	case room.EventWarning:
		if m.orchestrator != nil {
			m.syncRoomMetadata()
		}
		if strings.TrimSpace(event.Text) != "" {
			m.addNotice(waitStyle.Render(event.Text))
			m.status = "moderation warning"
		}
	case room.EventError:
		if event.Err != nil {
			m.addNotice(errorStyle.Render(event.Err.Error()))
			m.errorActivity(event.Participant, event.Err.Error())
			m.status = "agent error"
		}
	}
	if m.repliesOpen {
		m.refreshReplyViewport(true)
	}
	m.refreshContent()
}

func (m *Model) handlePlanDecisionKey(key tea.KeyMsg) bool {
	if m.room.PendingPlan == nil {
		return false
	}
	switch strings.ToLower(key.String()) {
	case "up", "down", "left", "right", "tab", "shift+tab":
		m.planChoice = (m.planChoice + 1) % 2
		m.status = "choose whether to implement or stay in Plan mode"
		return true
	case "y":
		m.planChoice = 0
	case "n", "esc":
		m.planChoice = 1
	case "enter":
	default:
		return false
	}
	if m.planChoice == 0 {
		if err := m.orchestrator.ExecutePendingPlan(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			m.status = "could not start plan implementation"
			return true
		}
		m.syncRoom()
		m.status = "accepted plan started in a fresh Default-mode workflow"
		return true
	}
	if err := m.orchestrator.DeclinePendingPlan(); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		m.status = "could not dismiss plan decision"
		return true
	}
	m.syncRoom()
	m.status = "staying in Plan mode; describe any revisions"
	return true
}

func (m *Model) handleDelegationDecisionKey(key tea.KeyMsg) bool {
	if m.room.PendingDelegation == nil {
		return false
	}
	switch strings.ToLower(key.String()) {
	case "up", "down", "left", "right", "tab", "shift+tab":
		m.delegationChoice = (m.delegationChoice + 1) % 2
		m.status = "choose whether to run the split or continue solo"
		return true
	case "y":
		m.delegationChoice = 0
	case "n", "esc":
		m.delegationChoice = 1
	case "enter":
	default:
		return false
	}
	if m.delegationChoice == 0 {
		if err := m.orchestrator.ApprovePendingDelegation(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			m.status = "could not start delegated split"
			return true
		}
		m.syncRoomMetadata()
		m.status = "delegated split approved and revalidating"
		return true
	}
	if err := m.orchestrator.DeclinePendingDelegation(); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		m.status = "could not continue solo"
		return true
	}
	m.syncRoomMetadata()
	m.status = "continuing with the requester solo"
	return true
}

func (m *Model) handleRouteDecisionKey(key tea.KeyMsg) bool {
	if len(m.room.PendingRoutes) == 0 {
		return false
	}
	const choices = 4
	switch strings.ToLower(key.String()) {
	case "up", "left", "shift+tab":
		m.routeReplaceConfirm = false
		m.routeChoice = (m.routeChoice - 1 + choices) % choices
		return true
	case "down", "right", "tab":
		m.routeReplaceConfirm = false
		m.routeChoice = (m.routeChoice + 1) % choices
		return true
	case "enter":
		if m.routeChoice == 2 && !m.routeReplaceConfirm {
			m.routeReplaceConfirm = true
			m.status = "press Enter again to replace and cancel current work"
			return true
		}
	case "esc":
		m.routeReplaceConfirm = false
		m.routeChoice = 3
	default:
		return false
	}
	sequence := m.room.PendingRoutes[0]
	var err error
	switch m.routeChoice {
	case 0:
		err = m.orchestrator.ResolveInput(sequence, chat.InputConversation, false)
	case 1:
		err = m.orchestrator.ResolveInput(sequence, chat.InputWork, false)
	case 2:
		err = m.orchestrator.ResolveInput(sequence, chat.InputWork, true)
	case 3:
		err = m.orchestrator.CancelPendingRoute(sequence)
	}
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		m.status = "routing choice failed"
	} else {
		m.syncRoom()
		m.routeChoice = 0
		m.routeReplaceConfirm = false
		m.status = "message routed"
	}
	return true
}

func cloneTurnRecords(values []chat.TurnRecord) []chat.TurnRecord {
	result := append([]chat.TurnRecord(nil), values...)
	for index := range result {
		result[index].Drafts = append([]string(nil), result[index].Drafts...)
		result[index].Tools = append([]string(nil), result[index].Tools...)
	}
	return result
}

func (m *Model) handleTurnDetailsShortcut(key tea.KeyMsg) bool {
	keyName := strings.ToLower(key.String())
	if keyName == "alt+t" {
		if m.streamMode.WithDefault() != chat.StreamHistory {
			m.addNotice("Turn details require /stream history.")
			return true
		}
		m.turnDetailsOpen = !m.turnDetailsOpen
		if m.turnDetailsOpen {
			m.turnIndex = len(m.turns) - 1
			m.refreshTurnViewport()
			m.status = "turn details open"
		} else {
			m.status = "turn details closed"
		}
		return true
	}
	if !m.turnDetailsOpen {
		return false
	}
	switch keyName {
	case "alt+left", "alt+right":
		if len(m.turns) == 0 {
			return true
		}
		delta := -1
		if keyName == "alt+right" {
			delta = 1
		}
		m.turnIndex = (m.turnIndex + delta + len(m.turns)) % len(m.turns)
		m.refreshTurnViewport()
		return true
	case "pgup", "pageup":
		m.turnViewport.PageUp()
		return true
	case "pgdown", "pagedown":
		m.turnViewport.PageDown()
		return true
	case "esc":
		m.turnDetailsOpen = false
		m.status = "turn details closed"
		return true
	}
	return false
}

func (m *Model) refreshTurnViewport() {
	if len(m.turns) == 0 {
		m.turnViewport.SetContent("No retained turn details yet.")
		return
	}
	if m.turnIndex < 0 || m.turnIndex >= len(m.turns) {
		m.turnIndex = len(m.turns) - 1
	}
	record := m.turns[m.turnIndex]
	lines := []string{
		fmt.Sprintf("TURN %d OF %d · @%s · %s", m.turnIndex+1, len(m.turns), record.Participant, strings.ToUpper(string(record.State))),
		fmt.Sprintf("%s → %s", record.StartedAt.Local().Format("2006-01-02 15:04:05"), record.CompletedAt.Local().Format("15:04:05")),
	}
	if record.Role != "" || record.Task != "" {
		lines = append(lines, strings.TrimSpace(record.Role+" · "+record.Task))
	}
	if record.FinalSequence > 0 {
		lines = append(lines, fmt.Sprintf("Published transcript message: #%d", record.FinalSequence))
		for _, message := range m.messages {
			if message.Sequence == record.FinalSequence {
				if final := strings.TrimSpace(message.Text); final != "" {
					heading := "FINAL RESPONSE"
					if record.State == chat.TurnRecordInterrupted {
						heading = "PUBLISHED RESPONSE BEFORE INTERRUPTION"
					}
					lines = append(lines, "", heading, final)
				}
				break
			}
		}
	}
	if len(record.Drafts) == 0 {
		lines = append(lines, "", "VISIBLE RESPONSE DRAFT", "No public draft text was retained.")
	} else {
		for index, draft := range record.Drafts {
			lines = append(lines, "", fmt.Sprintf("VISIBLE RESPONSE DRAFT %d", index+1), draft)
		}
	}
	if len(record.Tools) > 0 {
		lines = append(lines, "", "TOOL ACTIVITY")
		for _, detail := range record.Tools {
			lines = append(lines, "• "+detail)
		}
	}
	m.turnViewport.SetContent(strings.Join(lines, "\n"))
	m.turnViewport.GotoTop()
}

func (m Model) turnDetailsPanelView() string {
	if !m.turnDetailsOpen {
		return ""
	}
	footer := "Alt+←/→ turn · PgUp/PgDn scroll · Esc/Alt+T close"
	return modalStyle.Width(max(20, m.width-6)).Render(m.turnViewport.View() + "\n\n" + dimStyle.Render(footer))
}

func (m Model) liveResponseView() string {
	if m.streamMode.WithDefault() == chat.StreamStable || len(m.liveTurnIDs) == 0 {
		return ""
	}
	participants := make([]chat.Participant, 0, len(m.liveTurnIDs))
	for participant := range m.liveTurnIDs {
		participants = append(participants, participant)
	}
	participants = chat.OrderedParticipants(participants)
	lines := []string{waitStyle.Render("LIVE RESPONSE — PROVISIONAL")}
	for _, participant := range participants {
		state := m.liveStates[participant]
		status := "may change"
		switch state {
		case chat.TurnRecordFinal:
			status = "final response available · visible draft retained"
		case chat.TurnRecordSilent:
			status = "review completed without a public response"
		case chat.TurnRecordInterrupted:
			status = "turn interrupted · visible draft retained"
		}
		lines = append(lines, fmt.Sprintf("@%s · %s", participant, status))
		if text := strings.TrimSpace(publicLiveText(m.live[participant])); text != "" {
			wrapped := lipgloss.NewStyle().Width(max(20, m.width-10)).Render(text)
			lines = append(lines, limitVisibleLines(wrapped, 5))
		}
	}
	if m.streamMode.WithDefault() == chat.StreamHistory {
		lines = append(lines, dimStyle.Render("Alt+T opens retained Turn details"))
	}
	return modalStyle.Width(max(20, m.width-6)).Render(strings.Join(lines, "\n"))
}

func limitVisibleLines(value string, limit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return strings.Join(append(lines[:limit-1], "…"), "\n")
}

func (m *Model) handleConversationShortcut(key tea.KeyMsg) bool {
	keyName := strings.ToLower(key.String())
	if keyName == "alt+s" {
		m.repliesOpen = !m.repliesOpen
		if m.repliesOpen {
			m.selectInitialReply()
			m.refreshReplyViewport()
			m.status = "replies panel open"
		} else {
			m.status = "replies panel closed"
		}
		return true
	}
	if !m.repliesOpen {
		return false
	}
	indices := m.replyConversationIndices()
	if len(indices) == 0 {
		if keyName == "esc" {
			m.repliesOpen = false
			return true
		}
		return false
	}
	if m.replyIndex < 0 || m.replyIndex >= len(indices) {
		m.replyIndex = len(indices) - 1
	}
	if keyName == "up" || keyName == "down" {
		delta := -1
		if keyName == "down" {
			delta = 1
		}
		m.replyIndex = (m.replyIndex + delta + len(indices)) % len(indices)
		m.conversationReplace = ""
		m.refreshReplyViewport()
		return true
	}
	if keyName == "pgup" || keyName == "pageup" {
		m.replyViewport.PageUp()
		return true
	}
	if keyName == "pgdown" || keyName == "pagedown" {
		m.replyViewport.PageDown()
		return true
	}
	if keyName == "esc" {
		m.repliesOpen = false
		m.conversationReplace = ""
		return true
	}
	job := &m.room.Conversations[indices[m.replyIndex]]
	if m.conversationReplace != "" && keyName != "alt+r" {
		m.conversationReplace = ""
	}
	var err error
	switch keyName {
	case "alt+d":
		category := job.DerivedInboxCategory()
		if category != chat.ConversationInboxNewAnswer && category != chat.ConversationInboxActionNeeded {
			return false
		}
		err = m.orchestrator.DismissConversation(job.ID)
	case "alt+w":
		if job.DerivedInboxCategory() != chat.ConversationInboxActionNeeded {
			return false
		}
		err = m.orchestrator.PromoteConversation(job.ID, false)
	case "alt+r":
		if job.DerivedInboxCategory() != chat.ConversationInboxActionNeeded {
			return false
		}
		if m.conversationReplace != job.ID {
			m.conversationReplace = job.ID
			m.status = "press Alt+R again to replace and cancel current work"
			return true
		}
		m.conversationReplace = ""
		err = m.orchestrator.PromoteConversation(job.ID, true)
	case "alt+c":
		if job.DerivedInboxCategory() != chat.ConversationInboxWorking {
			return false
		}
		err = m.orchestrator.CancelConversation(job.ID)
	default:
		return false
	}
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
	} else {
		m.syncRoom()
		m.selectInitialReply()
		m.refreshReplyViewport()
	}
	return true
}

func (m Model) replyConversationIndices() []int {
	result := make([]int, 0, len(m.room.Conversations))
	for index := range m.room.Conversations {
		if m.room.Conversations[index].DerivedInboxCategory() != chat.ConversationInboxHidden {
			result = append(result, index)
		}
	}
	return result
}

func (m *Model) selectInitialReply() {
	indices := m.replyConversationIndices()
	m.replyIndex = -1
	for position, index := range indices {
		if m.room.Conversations[index].DerivedInboxCategory() == chat.ConversationInboxActionNeeded {
			m.replyIndex = position
			return
		}
	}
	var newest time.Time
	for position, index := range indices {
		job := m.room.Conversations[index]
		if job.DerivedInboxCategory() == chat.ConversationInboxNewAnswer && (m.replyIndex < 0 || job.UpdatedAt.After(newest)) {
			m.replyIndex = position
			newest = job.UpdatedAt
		}
	}
	if m.replyIndex >= 0 {
		return
	}
	for position, index := range indices {
		if m.room.Conversations[index].DerivedInboxCategory() == chat.ConversationInboxWorking {
			m.replyIndex = position
			return
		}
	}
}

func (m *Model) refreshReplyViewport(preserveOffset ...bool) {
	oldOffset := m.replyViewport.YOffset
	indices := m.replyConversationIndices()
	if len(indices) == 0 {
		m.replyViewport.SetContent("No room replies yet.")
		return
	}
	if m.replyIndex < 0 || m.replyIndex >= len(indices) {
		m.replyIndex = len(indices) - 1
	}
	job := m.room.Conversations[indices[m.replyIndex]]
	var questions, answers []string
	for _, message := range m.messages {
		if message.ConversationID != job.ID {
			continue
		}
		if message.Author == chat.User && strings.TrimSpace(message.Text) != "" {
			questions = append(questions, message.Text)
		} else if message.Author.ValidAgent() || message.Author == chat.System {
			if message.Kind == chat.MessageText && strings.TrimSpace(message.Text) != "" {
				answers = append(answers, message.Text)
			}
		}
	}
	state := strings.ReplaceAll(string(job.State), "_", " ")
	content := []string{
		fmt.Sprintf("REPLY %d OF %d · %s", m.replyIndex+1, len(indices), state),
		"",
		"QUESTION",
		strings.Join(questions, "\n\n"),
		"",
		"ANSWER",
		strings.Join(answers, "\n\n"),
	}
	if job.TerminalReason != "" {
		content = append(content, "", "ATTENTION", job.TerminalReason)
	}
	m.replyViewport.SetContent(strings.Join(content, "\n"))
	if len(preserveOffset) > 0 && preserveOffset[0] {
		m.replyViewport.YOffset = oldOffset
		if m.replyViewport.PastBottom() {
			m.replyViewport.GotoBottom()
		}
	} else {
		m.replyViewport.GotoTop()
	}
}

func (m Model) repliesPanelView() string {
	if !m.repliesOpen {
		return ""
	}
	actions := []string{"↑/↓ reply", "PgUp/PgDn scroll", "Esc/Alt+S close"}
	indices := m.replyConversationIndices()
	if len(indices) > 0 && m.replyIndex >= 0 && m.replyIndex < len(indices) {
		job := m.room.Conversations[indices[m.replyIndex]]
		switch job.DerivedInboxCategory() {
		case chat.ConversationInboxWorking:
			actions = append(actions, "Alt+C Cancel")
		case chat.ConversationInboxNewAnswer:
			actions = append(actions, "Alt+D Dismiss")
		case chat.ConversationInboxActionNeeded:
			actions = append(actions, "Alt+W Add", "Alt+R Replace", "Alt+D Dismiss")
		}
	}
	footer := strings.Join(actions, " · ")
	return modalStyle.Width(max(20, m.width-6)).Render(m.replyViewport.View() + "\n\n" + dimStyle.Render(footer))
}

func (m *Model) applySpeechEvent(event speech.Event) {
	m.speechState = event.State
	if event.Type == speech.EventError && event.Err != nil {
		m.addNotice(errorStyle.Render("Speech: " + event.Err.Error()))
		m.status = "speech playback failed"
	} else if event.Err != nil && m.status != "speech unavailable" {
		m.addNotice(errorStyle.Render("Speech unavailable: " + event.Err.Error()))
		m.status = "speech unavailable"
	}
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
		UpdatedAt: m.activityTime(),
	}
}

func (m *Model) setWorkAssignment(participant chat.Participant, role, task string) {
	if !participant.ValidAgent() {
		return
	}
	m.ensureActivityMap()
	current := m.activity[participant]
	current.Role = cleanActivityDetail(role)
	current.Task = cleanActivityDetail(task)
	current.UpdatedAt = m.activityTime()
	m.activity[participant] = current
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
	current.UpdatedAt = m.activityTime()
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
	current.UpdatedAt = m.activityTime()
	current.Role = ""
	current.Task = ""
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
	m.activity[participant] = participantActivity{Phase: phaseError, Detail: cleanActivityDetail(detail), UpdatedAt: m.activityTime()}
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

func (m Model) configuredParticipants() []chat.Participant {
	if m.orchestrator == nil {
		return chat.Agents()
	}
	settings := m.orchestrator.EffectiveSettings()
	participants := make([]chat.Participant, 0, len(settings))
	for participant := range settings {
		participants = append(participants, participant)
	}
	return chat.OrderedParticipants(participants)
}

func (m *Model) activityTime() time.Time {
	if m.now.IsZero() {
		m.now = time.Now()
	}
	return m.now
}

func isBusyPhase(phase activityPhase) bool {
	switch phase {
	case phaseQueued, phaseThinking, phaseResponding, phaseReading, phasePlanning, phaseEditing, phaseTesting, phaseWaiting, phaseQuiet, phaseApproval:
		return true
	default:
		return false
	}
}

func participantActivityFromStructured(value chat.ParticipantActivity, now time.Time) participantActivity {
	phase := activityPhase(value.State)
	switch value.State {
	case chat.SchedulerActive:
		phase = activityPhaseForDetail(value.Action)
		if phase == phaseThinking && value.Operation == chat.OperationWriting {
			phase = phaseResponding
		}
	case chat.SchedulerNeedsAttention:
		phase = phaseAttention
	case chat.SchedulerDone, chat.SchedulerIdle:
		phase = phaseIdle
	}
	if value.State == chat.SchedulerActive && !value.LastUpdateAt.IsZero() && now.Sub(value.LastUpdateAt) >= 90*time.Second {
		phase = phaseQuiet
	}
	detail := value.Action
	if value.WaitReason != "" && (value.State == chat.SchedulerQueued || value.State == chat.SchedulerWaiting || value.State == chat.SchedulerNeedsAttention) {
		detail = value.WaitReason
	} else if detail == "" {
		detail = value.WaitReason
	}
	return participantActivity{
		Phase: phase, Detail: detail, Role: value.Role, Task: value.Assignment,
		StartedAt: value.StartedAt, UpdatedAt: value.LastUpdateAt,
	}
}

func activityPhaseForDetail(detail string) activityPhase {
	value := strings.ToLower(cleanActivityDetail(detail))
	switch {
	case strings.Contains(value, "blocked") || strings.Contains(value, "cannot continue"):
		return phaseBlocked
	case strings.Contains(value, "go test") || strings.Contains(value, "make check") || strings.Contains(value, "test") || strings.Contains(value, "vet") || strings.Contains(value, "build"):
		return phaseTesting
	case strings.Contains(value, "apply_patch") || strings.Contains(value, "file change") || strings.Contains(value, "edit") || strings.Contains(value, "gofmt"):
		return phaseEditing
	case strings.Contains(value, "plan") || strings.Contains(value, "design"):
		return phasePlanning
	case strings.Contains(value, "wait") || strings.Contains(value, "watch"):
		return phaseWaiting
	case strings.Contains(value, "rg ") || strings.Contains(value, "sed ") || strings.Contains(value, "inspect") || strings.Contains(value, "read") || strings.Contains(value, "search"):
		return phaseReading
	default:
		return phaseThinking
	}
}

func (m *Model) resize() {
	workboardHeight := 0
	if board := m.activityView(); board != "" {
		workboardHeight = strings.Count(board, "\n") + 1
	}
	headerHeight := workboardHeight + 2
	textHeight := min(7, max(1, strings.Count(m.input.Value(), "\n")+1))
	m.input.SetHeight(textHeight)
	inputHeight := textHeight + 2 + m.composerItemsHeight()
	if m.room.PendingDelegation != nil {
		extra := 0
		if len(m.room.PendingDelegation.Joins) > 0 {
			extra++
		}
		if len(m.room.PendingDelegation.Leaves) > 0 {
			extra++
		}
		inputHeight = min(14, 6+len(m.room.PendingDelegation.Tasks)+extra)
	} else if m.room.PendingPlan != nil {
		inputHeight = 6
	} else if len(m.room.PendingRoutes) > 0 {
		inputHeight = 8
	}
	statusHeight := 2
	suggestionHeight := 0
	if m.room.PendingDelegation == nil && m.room.PendingPlan == nil && len(m.room.PendingRoutes) == 0 {
		if suggestions := m.suggestionsView(); suggestions != "" {
			suggestionHeight = strings.Count(suggestions, "\n") + 1
		}
	}
	modalHeight := 0
	if m.pending != nil || m.fullConfirmation != nil || m.room.Conflict != nil {
		modalHeight = 8
	}
	if m.repliesOpen {
		modalHeight += min(18, max(8, m.height/2))
	}
	if m.turnDetailsOpen {
		modalHeight += min(18, max(8, m.height/2))
	}
	liveHeight := 0
	if panel := m.liveResponseView(); panel != "" {
		liveHeight = strings.Count(panel, "\n") + 1
	}
	viewportHeight := m.height - headerHeight - inputHeight - statusHeight - modalHeight - suggestionHeight - liveHeight
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.viewport.Width = max(20, m.width)
	m.viewport.Height = viewportHeight
	m.replyViewport.Width = max(16, m.width-12)
	m.replyViewport.Height = min(14, max(5, m.height/2-5))
	m.turnViewport.Width = max(16, m.width-12)
	m.turnViewport.Height = min(14, max(5, m.height/2-5))
	m.input.SetWidth(max(10, m.width-2))
	m.ready = true
	m.refreshContent()
}

func (m *Model) refreshContent() {
	if !m.ready {
		return
	}
	width := max(20, m.viewport.Width-2)
	type timelineEntry struct {
		at    time.Time
		order int
		text  string
	}
	entries := make([]timelineEntry, 0, len(m.messages)+len(m.notices))
	pendingInputs := make(map[uint64]bool, len(m.room.PendingInputs))
	for _, sequence := range m.room.PendingInputs {
		pendingInputs[sequence] = true
	}
	pendingRoutes := make(map[uint64]bool, len(m.room.PendingRoutes))
	for _, sequence := range m.room.PendingRoutes {
		pendingRoutes[sequence] = true
	}
	for _, message := range m.messages {
		if message.Kind == chat.MessageTool || message.Kind == chat.MessageStatus || message.Kind == chat.MessageInterrupted {
			continue
		}
		var rendered strings.Builder
		label := m.participantLabel(message.Author, 0)
		if message.Author == chat.User && message.WorkflowMode.PlanOnly() {
			label += " " + planStyle.Render(" PLAN ")
		}
		if message.Author == chat.User && message.InputIntent == chat.InputWork {
			label += " " + userStyle.Render(" WORK ")
		}
		if message.Author == chat.User && message.InputIntent == chat.InputAmbiguous {
			label += " " + waitStyle.Render(" NEEDS ROUTING ")
		}
		if message.ConversationID != "" {
			label += " " + dimStyle.Render("CHAT")
		}
		if message.Target.ValidAgent() {
			label += dimStyle.Render(" → ") + m.participantLabel(message.Target, 0)
		}
		if message.Kind == chat.MessageInterrupted {
			label += waitStyle.Render(" (interrupted)")
		}
		timeLabel := dimStyle.Render(message.CreatedAt.Local().Format("15:04:05"))
		fmt.Fprintf(&rendered, "%s %s\n", label, timeLabel)
		bodyStyle := lipgloss.NewStyle().Width(width)
		if message.Kind == chat.MessageTool {
			bodyStyle = bodyStyle.Foreground(lipgloss.Color("244")).Italic(true)
		} else if message.Kind == chat.MessageInterrupted {
			bodyStyle = bodyStyle.Foreground(lipgloss.Color("214")).Italic(true)
		}
		if strings.TrimSpace(message.Text) != "" {
			text := message.Text
			if displayed, proposed := chat.DisplayProposedPlan(text); proposed {
				text = displayed
				rendered.WriteString(planStyle.Render(" PROPOSED PLAN "))
				rendered.WriteString("\n")
			}
			rendered.WriteString(bodyStyle.Render(text))
		}
		for index, attachment := range message.Attachments {
			if rendered.Len() > 0 && !strings.HasSuffix(rendered.String(), "\n") {
				rendered.WriteByte('\n')
			}
			dimensions := ""
			if attachment.Width > 0 && attachment.Height > 0 {
				dimensions = fmt.Sprintf(" · %d×%d", attachment.Width, attachment.Height)
			}
			rendered.WriteString(dimStyle.Render(fmt.Sprintf("[Image #%d%s]", index+1, dimensions)))
		}
		if pendingInputs[message.Sequence] {
			if rendered.Len() > 0 && !strings.HasSuffix(rendered.String(), "\n") {
				rendered.WriteByte('\n')
			}
			rendered.WriteString(waitStyle.Render("queued for after current work · use /steer to apply immediately"))
		}
		if pendingRoutes[message.Sequence] {
			if rendered.Len() > 0 && !strings.HasSuffix(rendered.String(), "\n") {
				rendered.WriteByte('\n')
			}
			rendered.WriteString(waitStyle.Render("needs routing · choose Answer as chat, Add to work, Replace, or Cancel below"))
		}
		rendered.WriteString("\n\n")
		entries = append(entries, timelineEntry{at: message.CreatedAt, order: int(message.Sequence), text: rendered.String()})
	}
	for index, notice := range m.notices {
		var rendered strings.Builder
		fmt.Fprintf(&rendered, "%s %s\n", systemStyle.Render("MOHUDDLE"), dimStyle.Render(notice.CreatedAt.Local().Format("15:04:05")))
		rendered.WriteString(lipgloss.NewStyle().Width(width).Render(notice.Text))
		rendered.WriteString("\n\n")
		entries = append(entries, timelineEntry{at: notice.CreatedAt, order: 1_000_000 + index, text: rendered.String()})
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].at.Equal(entries[right].at) {
			return entries[left].order < entries[right].order
		}
		return entries[left].at.Before(entries[right].at)
	})
	var value strings.Builder
	for _, entry := range entries {
		value.WriteString(entry.text)
	}
	oldOffset := m.viewport.YOffset
	m.viewport.SetContent(value.String())
	if m.preserveTranscriptOffset {
		m.viewport.YOffset = oldOffset
		m.following = false
		m.preserveTranscriptOffset = false
	} else if m.following {
		m.viewport.GotoBottom()
		m.unseen = 0
	} else {
		m.viewport.YOffset = oldOffset
		if m.viewport.PastBottom() {
			m.viewport.GotoBottom()
			m.following = true
			m.unseen = 0
		}
	}
}

func publicLiveText(value string) string {
	return agent.SanitizeResponseDraft(value)
}

func (m *Model) ensureLiveMaps() {
	if m.live == nil {
		m.live = make(map[chat.Participant]string)
	}
	if m.liveTurnIDs == nil {
		m.liveTurnIDs = make(map[chat.Participant]string)
	}
	if m.liveStates == nil {
		m.liveStates = make(map[chat.Participant]chat.TurnRecordState)
	}
}

func (m *Model) rememberTurn(record chat.TurnRecord) {
	for index := range m.turns {
		if m.turns[index].ID == record.ID {
			m.turns[index] = cloneTurnRecords([]chat.TurnRecord{record})[0]
			m.room.TurnHistory = cloneTurnRecords(m.turns)
			if m.turnDetailsOpen {
				m.refreshTurnViewport()
			}
			return
		}
	}
	m.turns = append(m.turns, cloneTurnRecords([]chat.TurnRecord{record})[0])
	if len(m.turns) > 40 {
		m.turns = append([]chat.TurnRecord(nil), m.turns[len(m.turns)-40:]...)
	}
	m.room.TurnHistory = cloneTurnRecords(m.turns)
	m.turnIndex = len(m.turns) - 1
	if m.turnDetailsOpen {
		m.refreshTurnViewport()
	}
}

func (m *Model) addNotice(value string) {
	m.notices = append(m.notices, noticeEntry{Text: value, CreatedAt: time.Now()})
	if !m.following {
		m.unseen++
	}
	if len(m.notices) > 100 {
		m.notices = m.notices[len(m.notices)-100:]
	}
	m.refreshContent()
}

func (m Model) View() string {
	if m.quitting || !m.ready {
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
	parts := []string{header}
	if board := m.activityView(); board != "" {
		parts = append(parts, board)
	}
	if live := m.liveResponseView(); live != "" {
		parts = append(parts, live)
	}
	parts = append(parts, m.viewport.View())
	if m.pending != nil {
		description := m.pending.Description
		if m.pending.Path != "" {
			description += "\nPath: " + m.pending.Path + " (" + string(m.pending.Mode) + ")"
		}
		choices := "[y] once  [a] this room  [n] deny  [x] stop turn"
		if m.pending.Kind == "directory_access" {
			choices = "[y] once  [a] this worker/room  [b] all workers/room  [n] deny  [x] stop"
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
	if m.room.PendingDelegation == nil && m.room.PendingPlan == nil {
		if items := m.composerItemsView(); items != "" {
			parts = append(parts, items)
		}
		if suggestions := m.suggestionsView(); suggestions != "" {
			parts = append(parts, suggestions)
		}
	}
	if pin := m.conversationPinView(); pin != "" {
		parts = append(parts, pin)
	}
	if panel := m.repliesPanelView(); panel != "" {
		parts = append(parts, panel)
	}
	if panel := m.turnDetailsPanelView(); panel != "" {
		parts = append(parts, panel)
	}
	parts = append(parts, m.composerView(), m.contextFooter(), m.keyFooter())
	return strings.Join(parts, "\n")
}

func (m Model) composerView() string {
	if m.room.PendingDelegation != nil {
		return m.delegationDecisionView()
	}
	if m.room.PendingPlan != nil {
		return m.planDecisionView()
	}
	if len(m.room.PendingRoutes) > 0 {
		return m.routeDecisionView()
	}
	return composerStyle.Width(max(10, m.width)).Render(withComposerBackground(m.input.View()))
}

func (m Model) routeDecisionView() string {
	sequence := m.room.PendingRoutes[0]
	text := "this message"
	for _, message := range m.messages {
		if message.Sequence == sequence {
			text = truncateDisplay(strings.Join(strings.Fields(message.Text), " "), 100)
			break
		}
	}
	choices := []string{"Answer as chat", "Add to work", "Replace current work", "Cancel"}
	lines := []string{lipgloss.NewStyle().Bold(true).Render("Should this be answered or implemented?"), dimStyle.Render(text)}
	for index, choice := range choices {
		prefix, style := "  ", dimStyle
		if index == m.routeChoice%len(choices) {
			prefix, style = "› ", userStyle
		}
		lines = append(lines, style.Render(prefix+choice))
	}
	if m.routeReplaceConfirm && m.routeChoice == 2 {
		lines = append(lines, errorStyle.Render("Press Enter again to replace and cancel current work."))
	} else {
		lines = append(lines, dimStyle.Render("↑/↓ select · Enter confirm · Esc cancels"))
	}
	return composerStyle.Width(max(10, m.width)).Render(strings.Join(lines, "\n"))
}

func (m Model) oldestUnreadConversation() *chat.ConversationJob {
	for index := range m.room.Conversations {
		job := &m.room.Conversations[index]
		if job.Unread && job.State == chat.ConversationAnswered {
			return job
		}
	}
	return nil
}

func (m Model) oldestActionableConversation() *chat.ConversationJob {
	if job := m.oldestUnreadConversation(); job != nil {
		return job
	}
	for index := range m.room.Conversations {
		if m.room.Conversations[index].DerivedInboxCategory() == chat.ConversationInboxActionNeeded {
			return &m.room.Conversations[index]
		}
	}
	return nil
}

func (m Model) conversationPinView() string {
	header := conversationInboxHeader(m.room.Conversations)
	if header == "" {
		return ""
	}
	return waitStyle.Render(header)
}

func conversationInboxHeader(jobs []chat.ConversationJob) string {
	counts := chat.CountConversationInbox(jobs)
	if counts.NewAnswers+counts.Working+counts.ActionNeeded == 0 {
		return ""
	}
	result := fmt.Sprintf("Replies: %d new", counts.NewAnswers)
	if counts.Working > 0 {
		result += fmt.Sprintf(" · %d working", counts.Working)
	}
	if counts.ActionNeeded > 0 {
		result += fmt.Sprintf(" · %d action needed", counts.ActionNeeded)
	}
	return result
}

func truncateDisplay(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}

func (m Model) planDecisionView() string {
	choices := []string{"Yes, implement this plan", "No, stay in Plan mode"}
	lines := []string{lipgloss.NewStyle().Bold(true).Render("Implement the plan?")}
	for index, choice := range choices {
		prefix := "  "
		style := dimStyle
		if index == m.planChoice%len(choices) {
			prefix = "› "
			style = userStyle
		}
		lines = append(lines, style.Render(prefix+choice))
	}
	lines = append(lines, dimStyle.Render("↑/↓ select · Enter confirm · Esc stays in Plan mode"))
	return composerStyle.Width(max(10, m.width)).Render(strings.Join(lines, "\n"))
}

func (m Model) delegationDecisionView() string {
	pending := m.room.PendingDelegation
	if pending == nil {
		return ""
	}
	lines := []string{lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("Run the split proposed by %s?", strings.ToUpper(string(pending.Requester))))}
	for _, task := range pending.Tasks {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("@%s · %s", task.Participant, truncateDisplay(strings.Join(strings.Fields(task.Task), " "), 100))))
	}
	if pending.ProviderLanes > 0 {
		laneLabel := "provider lanes"
		if pending.ProviderLanes == 1 {
			laneLabel = "provider lane"
		}
		detail := fmt.Sprintf("%d task(s) across %d %s", len(pending.Tasks), pending.ProviderLanes, laneLabel)
		if len(pending.Tasks) > 1 && pending.ProviderLanes == 1 {
			detail += " · tasks will run sequentially"
		} else if len(pending.Tasks) == 1 {
			detail += " · no parallel fan-out"
		} else if pending.ProviderLanes > 1 {
			detail += " · provider lanes can run concurrently"
		}
		lines = append(lines, dimStyle.Render(detail))
	}
	if len(pending.Joins) > 0 {
		lines = append(lines, dimStyle.Render("join: @"+strings.Join(participantStrings(pending.Joins), ", @")))
	}
	if len(pending.Leaves) > 0 {
		lines = append(lines, dimStyle.Render("leave: @"+strings.Join(participantStrings(pending.Leaves), ", @")))
	}
	choices := []string{"Run split", "Run solo"}
	for index, choice := range choices {
		prefix, style := "  ", dimStyle
		if index == m.delegationChoice%len(choices) {
			prefix, style = "› ", userStyle
		}
		lines = append(lines, style.Render(prefix+choice))
	}
	lines = append(lines, dimStyle.Render("↑/↓ select · Enter confirm · Y split · N/Esc solo"))
	return composerStyle.Width(max(10, m.width)).Render(strings.Join(lines, "\n"))
}

func participantStrings(participants []chat.Participant) []string {
	values := make([]string, 0, len(participants))
	for _, participant := range participants {
		values = append(values, string(participant))
	}
	return values
}

func withComposerBackground(view string) string {
	fill := lipgloss.NewStyle().Background(composerColor).Inline(true)
	const marker = "x"
	styledMarker := fill.Render(marker)
	markerIndex := strings.Index(styledMarker, marker)
	if markerIndex < 0 {
		return view
	}
	prefix := styledMarker[:markerIndex]
	suffix := styledMarker[markerIndex+len(marker):]
	if prefix == "" && suffix == "" {
		return view
	}
	lines := strings.Split(view, "\n")
	for index, line := range lines {
		lines[index] = reapplyTerminalStyle(line, prefix, suffix)
	}
	return strings.Join(lines, "\n")
}

func reapplyTerminalStyle(value, prefix, suffix string) string {
	if prefix == "" && suffix == "" {
		return value
	}
	if suffix != "" {
		value = strings.ReplaceAll(value, suffix, suffix+prefix)
	}
	return prefix + value + suffix
}

func (m Model) activityView() string {
	if m.progressMode.WithDefault() == chat.ProgressOff {
		return ""
	}
	participants := m.activityParticipants()
	lines := make([]string, 0, len(participants)+1)
	for _, participant := range participants {
		lines = append(lines, m.activityLine(participant))
	}
	if queued := len(m.room.PendingInputs); queued > 0 {
		lines = append(lines, waitStyle.Render(fmt.Sprintf("↳ QUEUED %d human message(s) · next safe boundary · /steer applies immediately", queued)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) activityLine(participant chat.Participant) string {
	activity := m.activity[participant]
	if activity.Phase == "" {
		activity.Phase = phaseIdle
	}

	displayPhase := activity.Phase
	if isBusyPhase(displayPhase) && displayPhase != phaseWaiting && displayPhase != phaseApproval && !activity.UpdatedAt.IsZero() && m.now.Sub(activity.UpdatedAt) >= 90*time.Second {
		displayPhase = phaseQuiet
	}
	icon := "○"
	phaseStyle := dimStyle
	switch {
	case displayPhase == phaseApproval:
		icon = "?"
		phaseStyle = waitStyle
	case displayPhase == phaseQuiet:
		icon = "·"
		phaseStyle = dimStyle
	case displayPhase == phaseError || displayPhase == phaseAttention:
		icon = "!"
		phaseStyle = errorStyle
	case displayPhase == phaseBlocked:
		icon = "!"
		phaseStyle = waitStyle
	case isBusyPhase(displayPhase):
		icon = activitySpinner[m.spinnerFrame%len(activitySpinner)]
		phaseStyle = busyStyle
	}

	label := m.participantLabel(participant, 7)
	line := fmt.Sprintf("%s %s", phaseStyle.Render(icon), label)
	if activity.Role != "" {
		line += dimStyle.Render(" " + activity.Role + " ·")
	}
	line += " " + phaseStyle.Render(string(displayPhase))
	if isBusyPhase(displayPhase) && !activity.StartedAt.IsZero() {
		line += dimStyle.Render("  " + formatElapsed(m.now.Sub(activity.StartedAt)))
	}
	if isBusyPhase(displayPhase) && !activity.UpdatedAt.IsZero() && m.now.Sub(activity.UpdatedAt) >= time.Second {
		line += dimStyle.Render("  last " + formatElapsed(m.now.Sub(activity.UpdatedAt)))
	}
	if activity.Detail != "" && m.width >= 48 {
		limit := max(12, m.width-42)
		line += dimStyle.Render("  · " + truncateActivityDetail(activity.Detail, limit))
	}
	if m.progressMode.WithDefault() == chat.ProgressDetailed && activity.Task != "" && m.width >= 64 {
		limit := max(12, m.width-42)
		line += dimStyle.Render("  · assignment: " + truncateActivityDetail(activity.Task, limit))
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
	return parseToggle(fields, current, "/details")
}

func parseToggle(fields []string, current bool, command string) (bool, error) {
	if len(fields) == 1 {
		return !current, nil
	}
	if len(fields) != 2 {
		return false, fmt.Errorf("usage: %s [on|off]", command)
	}
	switch strings.ToLower(fields[1]) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("usage: %s [on|off]", command)
	}
}

func parseWorkerCounts(fields []string, current map[chat.Participant]int) (map[chat.Participant]int, error) {
	usage := "usage: /workers [show|off|@all N|@provider N [@provider N ...]]"
	if len(fields) < 2 {
		return nil, fmt.Errorf("%s", usage)
	}
	if len(fields) == 2 && strings.EqualFold(fields[1], "off") {
		return map[chat.Participant]int{}, nil
	}
	if len(fields)%2 != 1 {
		return nil, fmt.Errorf("%s", usage)
	}
	next := make(map[chat.Participant]int, len(current))
	for provider, count := range current {
		if count > 0 {
			next[provider] = count
		}
	}
	seen := make(map[chat.Participant]bool)
	for index := 1; index < len(fields); index += 2 {
		target := strings.ToLower(fields[index])
		count, err := strconv.Atoi(fields[index+1])
		if err != nil || count < 0 {
			return nil, fmt.Errorf("worker count for %s must be a non-negative integer", fields[index])
		}
		if target == "@all" {
			if len(fields) != 3 {
				return nil, fmt.Errorf("@all cannot be combined with provider-specific worker counts")
			}
			next = make(map[chat.Participant]int, len(chat.Agents()))
			for _, provider := range chat.Agents() {
				if count > 0 {
					next[provider] = count
				}
			}
			break
		}
		provider, ok := chat.ParseParticipant(strings.TrimPrefix(target, "@"))
		if !ok || !strings.HasPrefix(target, "@") || !provider.IsPrimaryAgent() {
			return nil, fmt.Errorf("%s is not a primary provider; use @codex, @claude, @agy, or @copilot", fields[index])
		}
		if seen[provider] {
			return nil, fmt.Errorf("worker count for @%s was specified more than once", provider)
		}
		seen[provider] = true
		if count == 0 {
			delete(next, provider)
		} else {
			next[provider] = count
		}
	}
	if err := appsettings.ValidateWorkerCounts(next); err != nil {
		return nil, err
	}
	return next, nil
}

func parseDelegation(value string) (chat.Participant, string, error) {
	rest := strings.TrimSpace(value)
	if index := strings.IndexAny(rest, " \t\r\n"); index >= 0 {
		rest = strings.TrimSpace(rest[index:])
	} else {
		rest = ""
	}
	index := strings.IndexAny(rest, " \t\r\n")
	if index < 0 {
		return "", "", fmt.Errorf("usage: /delegate @agent TASK")
	}
	target := rest[:index]
	task := strings.TrimSpace(rest[index:])
	participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(target, "@")))
	if !ok || !strings.HasPrefix(target, "@") || task == "" {
		return "", "", fmt.Errorf("usage: /delegate @agent TASK (for example, /delegate @codex-1 inspect the parser)")
	}
	return participant, task, nil
}

func (m *Model) handleRoster(fields []string, now time.Time) {
	usage := "usage: /roster [show|schedule join|leave @agent for DURATION|at RFC3339|retry [REASON]|cancel ID]"
	if len(fields) == 1 || (len(fields) == 2 && strings.EqualFold(fields[1], "show")) {
		m.showRosterActions()
		return
	}
	switch strings.ToLower(fields[1]) {
	case "cancel":
		if len(fields) != 3 {
			m.addNotice(errorStyle.Render(usage))
			return
		}
		if err := m.orchestrator.CancelRosterAction(fields[2]); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.status = "scheduled roster action cancelled"
		m.showRosterActions()
	case "schedule":
		if len(fields) < 5 {
			m.addNotice(errorStyle.Render(usage))
			return
		}
		action := chat.RosterActionType(strings.ToLower(fields[2]))
		participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(fields[3], "@")))
		if !action.Valid() || !ok || !strings.HasPrefix(fields[3], "@") {
			m.addNotice(errorStyle.Render(usage))
			return
		}
		index := 5
		var executeAt time.Time
		switch strings.ToLower(fields[4]) {
		case "for":
			if len(fields) <= index {
				m.addNotice(errorStyle.Render("scheduled roster duration is missing"))
				return
			}
			duration, err := time.ParseDuration(fields[index])
			if err != nil || duration <= 0 {
				m.addNotice(errorStyle.Render("scheduled roster duration must be positive, for example 30m or 2h"))
				return
			}
			executeAt = now.Add(duration)
			index++
		case "at":
			if len(fields) <= index {
				m.addNotice(errorStyle.Render("scheduled roster time is missing"))
				return
			}
			parsed, err := time.Parse(time.RFC3339, fields[index])
			if err != nil {
				m.addNotice(errorStyle.Render("scheduled roster time must use RFC3339, for example 2026-08-23T18:30:00-04:00"))
				return
			}
			executeAt = parsed
			index++
		case "retry":
			if action != chat.RosterActionJoin {
				m.addNotice(errorStyle.Render("retry scheduling is valid only for a join action"))
				return
			}
			index = 5
			roomState, _ := m.orchestrator.Snapshot()
			availability, unavailable := roomState.Availability[participant]
			if !unavailable || availability.RetryAt == nil || !availability.RetryAt.After(now) {
				m.addNotice(errorStyle.Render(fmt.Sprintf("@%s has no future confirmed retry time", participant)))
				return
			}
			executeAt = *availability.RetryAt
		default:
			m.addNotice(errorStyle.Render(usage))
			return
		}
		reason := strings.TrimSpace(strings.Join(fields[index:], " "))
		if _, err := m.orchestrator.ScheduleRosterAction(action, participant, executeAt, reason); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.status = fmt.Sprintf("scheduled %s for %s", action, participant)
		m.showRosterActions()
	default:
		m.addNotice(errorStyle.Render(usage))
	}
}

func (m *Model) showRosterActions() {
	if err := m.orchestrator.RefreshRosterActions(); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	actions := m.orchestrator.RosterActions()
	if len(actions) == 0 {
		m.addNotice("No scheduled roster actions")
		return
	}
	const displayLimit = 20
	start := 0
	if len(actions) > displayLimit {
		start = len(actions) - displayLimit
	}
	lines := []string{"Scheduled roster actions (latest first):"}
	for index := len(actions) - 1; index >= start; index-- {
		lines = append(lines, formatRosterAction(actions[index]))
	}
	if start > 0 {
		lines = append(lines, fmt.Sprintf("%d older audit records omitted", start))
	}
	m.addNotice(strings.Join(lines, "\n"))
}

func (m *Model) handleRemote(fields []string, _ string) {
	if m.remoteDevices == nil {
		m.addNotice(errorStyle.Render("remote phone access is disabled; restart with --remote-listen"))
		return
	}
	if len(fields) == 1 || (len(fields) == 2 && (strings.EqualFold(fields[1], "show") || strings.EqualFold(fields[1], "devices"))) {
		m.showRemoteDevices()
		return
	}
	switch strings.ToLower(fields[1]) {
	case "pair":
		if len(fields) < 4 {
			m.addNotice(errorStyle.Render("usage: /remote pair observe|participate|admin DEVICE_NAME"))
			return
		}
		var scopes []device.Scope
		switch strings.ToLower(fields[2]) {
		case "observe":
			scopes = []device.Scope{device.ScopeObserve}
		case "participate":
			scopes = []device.Scope{device.ScopeObserve, device.ScopeParticipate}
		case "admin":
			scopes = []device.Scope{device.ScopeObserve, device.ScopeParticipate, device.ScopeAdmin}
		default:
			m.addNotice(errorStyle.Render("remote scope must be observe, participate, or admin"))
			return
		}
		name := strings.TrimSpace(strings.Join(fields[3:], " "))
		invitation, err := m.remoteDevices.CreateInvitation(m.room.ID, name, scopes, 15*time.Minute)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		pairURL := m.remoteOrigin + "/#code=" + invitation.Code
		m.addNotice(fmt.Sprintf("Remote device invitation\nname: %s\nscope: %s\nexpires: %s\ncode: %s\nopen: %s\nThe code is single-use. The remote execution ceiling remains read-only.", name, fields[2], invitation.ExpiresAt.Local().Format(time.RFC3339), invitation.Code, pairURL))
		m.status = "remote pairing invitation created"
	case "revoke":
		if len(fields) != 3 {
			m.addNotice(errorStyle.Render("usage: /remote revoke DEVICE_ID"))
			return
		}
		grant, ok := remoteGrant(m.remoteDevices.List(), fields[2])
		if !ok {
			m.addNotice(errorStyle.Render("remote device not found"))
			return
		}
		if err := m.remoteDevices.Revoke(grant.ID); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		if m.remoteAudit != nil {
			scopes := make([]api.Scope, 0, len(grant.Scopes))
			for _, scope := range grant.Scopes {
				scopes = append(scopes, api.Scope(scope))
			}
			_ = m.remoteAudit.Append(api.AuditRecord{Action: "remote.revoke", DeviceID: grant.ID, RoomID: grant.RoomID, Scopes: scopes, Permission: string(grant.PermissionCeiling), Allowed: true, Identity: "trusted-local-tui"})
		}
		m.status = "remote device revoked"
		m.addNotice("Revoked remote device " + grant.Name + " (" + displayID(grant.ID) + "); active sessions were closed.")
	case "scope":
		if len(fields) != 4 {
			m.addNotice(errorStyle.Render("usage: /remote scope DEVICE_ID observe|participate|admin"))
			return
		}
		var scopes []device.Scope
		switch strings.ToLower(fields[3]) {
		case "observe":
			scopes = []device.Scope{device.ScopeObserve}
		case "participate":
			scopes = []device.Scope{device.ScopeObserve, device.ScopeParticipate}
		case "admin":
			scopes = []device.Scope{device.ScopeObserve, device.ScopeParticipate, device.ScopeAdmin}
		default:
			m.addNotice(errorStyle.Render("remote scope must be observe, participate, or admin"))
			return
		}
		grant, err := m.remoteDevices.SetScopes(fields[2], scopes)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		if m.remoteAudit != nil {
			apiScopes := make([]api.Scope, 0, len(scopes))
			for _, scope := range scopes {
				apiScopes = append(apiScopes, api.Scope(scope))
			}
			_ = m.remoteAudit.Append(api.AuditRecord{Action: "remote.scope", DeviceID: grant.ID, RoomID: grant.RoomID, Scopes: apiScopes, Permission: string(grant.PermissionCeiling), Allowed: true, Identity: "trusted-local-tui"})
		}
		m.status = "remote device scope updated"
		m.addNotice(fmt.Sprintf("Remote device %s now has %s scope; prior sessions were closed. Agent requests remain read-only.", grant.Name, fields[3]))
	case "audit":
		m.showRemoteAudit()
	default:
		m.addNotice(errorStyle.Render("usage: /remote [devices|pair observe|participate|admin NAME|scope DEVICE_ID observe|participate|admin|revoke DEVICE_ID|audit]"))
	}
}

func (m *Model) showRemoteDevices() {
	grants := m.remoteDevices.List()
	lines := []string{"Remote phone gateway: " + m.remoteOrigin, "Execution ceiling: read-only"}
	if len(grants) == 0 {
		lines = append(lines, "No paired devices")
	}
	for _, grant := range grants {
		state := "active"
		if !grant.Active() {
			state = "revoked " + grant.RevokedAt.Local().Format(time.RFC3339)
		}
		scopes := make([]string, 0, len(grant.Scopes))
		for _, scope := range grant.Scopes {
			scopes = append(scopes, string(scope))
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s  created %s", grant.ID, grant.Name, strings.Join(scopes, ","), state, grant.CreatedAt.Local().Format(time.RFC3339)))
	}
	m.addNotice(strings.Join(lines, "\n"))
}

func (m *Model) showRemoteAudit() {
	if m.remoteAudit == nil {
		m.addNotice("Remote audit is unavailable")
		return
	}
	records, err := m.remoteAudit.Recent(50)
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	lines := []string{"Remote phone audit (latest 20):"}
	shown := 0
	for index := len(records) - 1; index >= 0 && shown < 20; index-- {
		record := records[index]
		if !strings.HasPrefix(record.Action, "remote.") && record.DeviceID == "" {
			continue
		}
		state := "denied"
		if record.Allowed {
			state = "allowed"
		}
		detail := fmt.Sprintf("%s  %s  %s  device %s", record.At.Local().Format(time.RFC3339), record.Action, state, displayID(record.DeviceID))
		if record.Error != "" {
			detail += "; " + record.Error
		}
		lines = append(lines, detail)
		shown++
	}
	if shown == 0 {
		lines = append(lines, "No remote audit records")
	}
	m.addNotice(strings.Join(lines, "\n"))
}

func remoteDeviceCounts(grants []device.Grant) (active, revoked int) {
	for _, grant := range grants {
		if grant.Active() {
			active++
		} else {
			revoked++
		}
	}
	return active, revoked
}

func remoteGrant(grants []device.Grant, id string) (device.Grant, bool) {
	id = strings.TrimSpace(id)
	for _, grant := range grants {
		if grant.ID == id {
			return grant, true
		}
	}
	return device.Grant{}, false
}

func formatRosterAction(action chat.ScheduledRosterAction) string {
	detail := fmt.Sprintf("[%s] %s %s @%s at %s; authorized by %s", action.Status, action.ID, action.Action, action.Participant, action.ExecuteAt.Local().Format(time.RFC3339), action.AuthorizedBy)
	if action.Reason != "" {
		detail += "; reason: " + action.Reason
	}
	if action.CompletedAt != nil {
		detail += "; completed " + action.CompletedAt.Local().Format(time.RFC3339)
	}
	if action.Detail != "" {
		detail += "; " + action.Detail
	}
	return detail
}

func settingsTargetAll(fields []string) bool {
	index := 1
	if len(fields) > index && strings.EqualFold(fields[index], "default") {
		index++
	}
	return len(fields) > index && strings.EqualFold(fields[index], "@all")
}

func workerCountsSummary(counts map[chat.Participant]int) string {
	parts := make([]string, 0, len(chat.Agents()))
	total := 0
	for _, provider := range chat.Agents() {
		count := counts[provider]
		total += count
		parts = append(parts, fmt.Sprintf("%s %d", provider, count))
	}
	return fmt.Sprintf("Auxiliary workers: %d total (%s)", total, strings.Join(parts, "; "))
}

func workerCountsEqual(left, right map[chat.Participant]int) bool {
	for _, provider := range chat.Agents() {
		if left[provider] != right[provider] {
			return false
		}
	}
	return true
}

func rosterParticipants(installed []chat.Participant) []chat.Participant {
	seen := make(map[chat.Participant]bool, len(chat.Agents())+len(installed))
	values := make([]chat.Participant, 0, len(chat.Agents())+len(installed))
	for _, participant := range append(chat.Agents(), installed...) {
		if participant.ValidAgent() && !seen[participant] {
			seen[participant] = true
			values = append(values, participant)
		}
	}
	return chat.OrderedParticipants(values)
}

func configuredRosterParticipants(installed []chat.Participant, counts map[chat.Participant]int) []chat.Participant {
	return rosterParticipants(append(installed, appsettings.WorkerParticipants(counts)...))
}

func conflictSummary(conflict *chat.ConflictState) string {
	if conflict == nil {
		return "material disagreement"
	}
	parts := make([]string, 0, len(conflict.Reasons))
	participants := make([]chat.Participant, 0, len(conflict.Reasons))
	for participant := range conflict.Reasons {
		participants = append(participants, participant)
	}
	for _, participant := range chat.OrderedParticipants(participants) {
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
	completionSound := "off"
	if m.completionSound {
		completionSound = "on"
	}
	lines := []string{
		"Agent settings (effective; personal default):",
		"Host-mediated public web research: " + onOff(m.orchestrator.WebSearchEnabled()) + " (/search [on|off|status])",
		"Progress workboard: " + string(m.progressMode.WithDefault()) + " (/progress [compact|detailed|off])",
		"Response previews: " + string(m.streamMode.WithDefault()) + " (/stream [stable|live|history]; Alt+T opens history)",
		"Behind-the-scenes details: " + details + " (/details [on|off])",
		"AI-finished terminal sound: " + completionSound + " (/sound [on|off])",
		workerCountsSummary(m.orchestrator.WorkerCounts()) + " (/workers)",
	}
	if m.remoteDevices != nil {
		lines = append(lines, "Remote phone gateway: "+m.remoteOrigin+"; fixed read-only execution ceiling (/remote)")
	} else {
		lines = append(lines, "Remote phone gateway: disabled (start with --remote-listen)")
	}
	for _, participant := range m.configuredParticipants() {
		scope := "inherits default"
		if _, ok := roomState.Settings[participant]; ok {
			scope = "room override"
		}
		effectiveSummary := settingsSummary(effective[participant])
		defaultSummary := settingsSummary(defaults[participant])
		lines = append(lines, fmt.Sprintf("%-13s %s (%s)\n              default: %s", m.plainParticipantLabel(participant), effectiveSummary, scope, defaultSummary))
	}
	lines = append(lines,
		"Set room: /model @codex MODEL | /effort @agy LEVEL | /permissions @agent PROFILE",
		"Set personal default: /model default @codex MODEL (same form for effort/permissions)",
		"Remove room override: /inherit @agent|@all",
		"Models accept provider aliases or full IDs. Use default/auto to clear model/effort overrides.")
	m.addNotice(strings.Join(lines, "\n"))
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (m *Model) handleCore(fields []string) {
	if len(fields) == 1 || (len(fields) == 2 && (strings.EqualFold(fields[1], "show") || strings.EqualFold(fields[1], "status"))) {
		m.showCoreStatus()
		return
	}
	subcommand := strings.ToLower(fields[1])
	switch subcommand {
	case "inherit":
		if len(fields) != 2 {
			m.addNotice(errorStyle.Render("usage: /core inherit"))
			return
		}
		if err := m.orchestrator.InheritCorePolicy(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.syncRoomMetadata()
		m.status = "core policy now inherits personal defaults"
		m.showCoreStatus()
		return
	case "preferred", "fallbacks", "failover", "restoration":
		personalDefault := len(fields) > 2 && strings.EqualFold(fields[2], "default")
		valueIndex := 2
		if personalDefault {
			valueIndex++
		}
		policy := m.orchestrator.CoreStatus().Policy
		if personalDefault {
			policy = m.orchestrator.DefaultCorePolicy()
		}
		if len(fields) <= valueIndex {
			m.addNotice(errorStyle.Render("usage: /core " + subcommand + " [default] VALUE"))
			return
		}
		switch subcommand {
		case "preferred":
			participants, err := parseCoreParticipants(fields[valueIndex:], false)
			if err != nil || len(participants) == 0 {
				m.addNotice(errorStyle.Render("usage: /core preferred [default] @agent [@agent ...]"))
				return
			}
			policy.Preferred = participants
			policy.Fallbacks = withoutCoreParticipants(policy.Fallbacks, participants)
		case "fallbacks":
			participants, err := parseCoreParticipants(fields[valueIndex:], true)
			if err != nil {
				m.addNotice(errorStyle.Render("usage: /core fallbacks [default] @agent [@agent ...]|none"))
				return
			}
			policy.Fallbacks = participants
		case "failover":
			if len(fields) != valueIndex+1 {
				m.addNotice(errorStyle.Render("usage: /core failover [default] auto|prompt|off"))
				return
			}
			mode := chat.CoreFailoverMode(strings.ToLower(fields[valueIndex]))
			if !mode.Valid() {
				m.addNotice(errorStyle.Render("core failover must be auto, prompt, or off"))
				return
			}
			policy.Failover = mode
		case "restoration":
			if len(fields) != valueIndex+1 {
				m.addNotice(errorStyle.Render("usage: /core restoration [default] auto|prompt|manual"))
				return
			}
			mode := chat.CoreRestoreMode(strings.ToLower(fields[valueIndex]))
			if !mode.Valid() {
				m.addNotice(errorStyle.Render("core restoration must be auto, prompt, or manual"))
				return
			}
			policy.Restore = mode
		}
		if err := m.orchestrator.SetCorePolicy(policy, personalDefault); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.syncRoomMetadata()
		m.status = "core policy updated"
		m.showCoreStatus()
		return
	case "promote":
		if len(fields) != 3 {
			m.addNotice(errorStyle.Render("usage: /core promote @agent"))
			return
		}
		participant, ok := parseCoreParticipant(fields[2])
		if !ok {
			m.addNotice(errorStyle.Render("usage: /core promote @agent"))
			return
		}
		if err := m.orchestrator.PromoteCore(participant, ""); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	case "replace":
		if len(fields) != 4 {
			m.addNotice(errorStyle.Render("usage: /core replace @preferred @fallback"))
			return
		}
		replaced, replacedOK := parseCoreParticipant(fields[2])
		participant, participantOK := parseCoreParticipant(fields[3])
		if !replacedOK || !participantOK {
			m.addNotice(errorStyle.Render("usage: /core replace @preferred @fallback"))
			return
		}
		if err := m.orchestrator.PromoteCore(participant, replaced); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	case "demote":
		if len(fields) != 3 {
			m.addNotice(errorStyle.Render("usage: /core demote @agent"))
			return
		}
		participant, ok := parseCoreParticipant(fields[2])
		if !ok {
			m.addNotice(errorStyle.Render("usage: /core demote @agent"))
			return
		}
		if err := m.orchestrator.RestoreCore(participant); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	case "restore":
		var participant chat.Participant
		if len(fields) == 3 && !strings.EqualFold(fields[2], "all") && !strings.EqualFold(fields[2], "@all") {
			var ok bool
			participant, ok = parseCoreParticipant(fields[2])
			if !ok {
				m.addNotice(errorStyle.Render("usage: /core restore [@agent|all]"))
				return
			}
		} else if len(fields) > 3 {
			m.addNotice(errorStyle.Render("usage: /core restore [@agent|all]"))
			return
		}
		if err := m.orchestrator.RestoreCore(participant); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	case "unavailable":
		participant, availability, err := parseCoreAvailability(fields, time.Now())
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		if err := m.orchestrator.SetParticipantAvailability(participant, &availability); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	case "available":
		if len(fields) != 3 {
			m.addNotice(errorStyle.Render("usage: /core available @agent"))
			return
		}
		participant, ok := parseCoreParticipant(fields[2])
		if !ok {
			m.addNotice(errorStyle.Render("usage: /core available @agent"))
			return
		}
		if err := m.orchestrator.SetParticipantAvailability(participant, nil); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
	default:
		m.addNotice(errorStyle.Render("usage: /core [show|preferred|fallbacks|failover|restoration|promote|replace|demote|restore|unavailable|available|inherit]"))
		return
	}
	m.syncRoomMetadata()
	m.status = "core roster updated"
	m.showCoreStatus()
}

func parseCoreParticipant(value string) (chat.Participant, bool) {
	if !strings.HasPrefix(value, "@") {
		return "", false
	}
	return chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(value, "@")))
}

func parseCoreParticipants(values []string, allowNone bool) ([]chat.Participant, error) {
	if allowNone && len(values) == 1 && strings.EqualFold(values[0], "none") {
		return []chat.Participant{}, nil
	}
	result := make([]chat.Participant, 0, len(values))
	seen := make(map[chat.Participant]bool)
	for _, value := range values {
		participant, ok := parseCoreParticipant(value)
		if !ok || seen[participant] {
			return nil, fmt.Errorf("invalid or duplicate core peer %q", value)
		}
		seen[participant] = true
		result = append(result, participant)
	}
	return result, nil
}

func withoutCoreParticipants(values, removed []chat.Participant) []chat.Participant {
	excluded := make(map[chat.Participant]bool, len(removed))
	for _, participant := range removed {
		excluded[participant] = true
	}
	result := make([]chat.Participant, 0, len(values))
	for _, participant := range values {
		if !excluded[participant] {
			result = append(result, participant)
		}
	}
	return result
}

func parseCoreAvailability(fields []string, now time.Time) (chat.Participant, chat.ParticipantAvailability, error) {
	usage := "usage: /core unavailable @agent [for DURATION|until RFC3339] [REASON]"
	if len(fields) < 3 {
		return "", chat.ParticipantAvailability{}, fmt.Errorf("%s", usage)
	}
	participant, ok := parseCoreParticipant(fields[2])
	if !ok {
		return "", chat.ParticipantAvailability{}, fmt.Errorf("%s", usage)
	}
	availability := chat.ParticipantAvailability{
		Reason: "manually marked unavailable", Source: "manual", DetectedAt: now.UTC(), Confidence: "confirmed",
	}
	index := 3
	if len(fields) > index {
		switch strings.ToLower(fields[index]) {
		case "for":
			index++
			if len(fields) <= index {
				return "", chat.ParticipantAvailability{}, fmt.Errorf("availability duration is missing")
			}
			duration, err := time.ParseDuration(fields[index])
			if err != nil || duration <= 0 {
				return "", chat.ParticipantAvailability{}, fmt.Errorf("availability duration must be a positive Go duration such as 2h or 30m")
			}
			retryAt := now.Add(duration).UTC()
			availability.RetryAt = &retryAt
			index++
		case "until":
			index++
			if len(fields) <= index {
				return "", chat.ParticipantAvailability{}, fmt.Errorf("availability retry time is missing")
			}
			retryAt, err := time.Parse(time.RFC3339, fields[index])
			if err != nil {
				return "", chat.ParticipantAvailability{}, fmt.Errorf("retry time must use RFC3339, for example 2026-08-23T01:20:00-04:00")
			}
			if !retryAt.After(now) {
				return "", chat.ParticipantAvailability{}, fmt.Errorf("retry time must be in the future")
			}
			retryAt = retryAt.UTC()
			availability.RetryAt = &retryAt
			index++
		default:
			if duration, err := time.ParseDuration(fields[index]); err == nil && duration > 0 {
				retryAt := now.Add(duration).UTC()
				availability.RetryAt = &retryAt
				index++
			}
		}
	}
	if reason := strings.TrimSpace(strings.Join(fields[index:], " ")); reason != "" {
		availability.Reason = reason
	}
	return participant, availability, nil
}

func (m *Model) showCoreStatus() {
	if err := m.orchestrator.RefreshCoreState(); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	status := m.orchestrator.CoreStatus()
	roomState, _ := m.orchestrator.Snapshot()
	installed := make(map[chat.Participant]bool)
	for _, participant := range m.orchestrator.Participants() {
		installed[participant] = true
	}
	scope := "room override"
	if status.Inherited {
		scope = "personal default"
	}
	lines := []string{
		"Core peers (" + scope + "):",
		"preferred: " + formatCoreParticipants(status.Policy.Preferred),
		"fallbacks: " + formatCoreParticipants(status.Policy.Fallbacks),
		fmt.Sprintf("failover: %s; restoration: %s", status.Policy.Failover, status.Policy.Restore),
		"active: " + formatCoreParticipants(status.Active),
		fmt.Sprintf("moderator: @%s; selection: %s", displayModerator(status.Moderator), formatModeratorSelection(status)),
	}
	for _, promotion := range status.Promotions {
		detail := fmt.Sprintf("temporary: @%s", promotion.Participant)
		if promotion.Replaces.ValidAgent() {
			detail += " replaces @" + string(promotion.Replaces)
		}
		detail += fmt.Sprintf(" (%s", promotion.Source)
		if strings.TrimSpace(promotion.Reason) != "" {
			detail += ": " + promotion.Reason
		}
		detail += ")"
		lines = append(lines, detail)
		if promotion.Replaces.ValidAgent() && roomState.Present(promotion.Replaces) && installed[promotion.Replaces] {
			if _, unavailable := status.Availability[promotion.Replaces]; !unavailable && status.Policy.Restore != chat.CoreRestoreAuto {
				lines = append(lines, fmt.Sprintf("pending restoration: @%s is available; use /core restore @%s", promotion.Replaces, promotion.Replaces))
			}
		}
	}
	if status.Policy.Failover == chat.CoreFailoverPrompt {
		replaced := make(map[chat.Participant]bool)
		for _, promotion := range status.Promotions {
			replaced[promotion.Replaces] = true
		}
		for _, participant := range status.Policy.Preferred {
			_, unavailable := status.Availability[participant]
			if !replaced[participant] && (!roomState.Present(participant) || !installed[participant] || unavailable) {
				lines = append(lines, fmt.Sprintf("pending failover: @%s needs a replacement; use /core replace @%s @fallback", participant, participant))
			}
		}
	}
	for _, participant := range chat.Agents() {
		availability, ok := status.Availability[participant]
		if !ok {
			continue
		}
		detail := fmt.Sprintf("unavailable: @%s — %s", participant, availability.Reason)
		if availability.RetryAt != nil {
			detail += "; retry " + availability.RetryAt.Local().Format(time.RFC3339)
		}
		lines = append(lines, detail)
	}
	lines = append(lines, "Use /core preferred|fallbacks [default] …, /core replace @preferred @fallback, /core promote|demote @agent, or /core unavailable|available @agent.")
	m.addNotice(strings.Join(lines, "\n"))
}

func formatCoreParticipants(participants []chat.Participant) string {
	if len(participants) == 0 {
		return "none"
	}
	values := make([]string, 0, len(participants))
	for _, participant := range participants {
		values = append(values, "@"+string(participant))
	}
	return strings.Join(values, ", ")
}

func formatModeratorSelection(status room.CoreStatus) string {
	if !status.ModeratorExplicit {
		return "auto"
	}
	return "@" + string(status.ModeratorPreference)
}

func (m *Model) showAgents() {
	if err := m.orchestrator.RefreshCoreState(); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	roomState, _ := m.orchestrator.Snapshot()
	coreStatus := m.orchestrator.CoreStatus()
	available := make(map[chat.Participant]bool)
	for _, participant := range m.orchestrator.Participants() {
		available[participant] = true
	}
	lines := []string{"Room roster:"}
	for _, participant := range configuredRosterParticipants(m.orchestrator.Participants(), m.orchestrator.WorkerCounts()) {
		state := "unavailable (CLI not found)"
		if available[participant] {
			state = "away"
			if roomState.Present(participant) {
				state = "present"
			}
		}
		role := "optional peer"
		if participant.IsAuxiliary() {
			role = "auxiliary worker"
		} else if containsCoreParticipant(coreStatus.Policy.Preferred, participant) {
			role = "preferred core"
		} else if containsCoreParticipant(coreStatus.Policy.Fallbacks, participant) {
			role = "fallback peer"
		}
		for _, promotion := range coreStatus.Promotions {
			if promotion.Participant != participant {
				continue
			}
			role = "temporary core"
			if promotion.Replaces.ValidAgent() {
				role += " for " + string(promotion.Replaces)
			}
			if promotion.Replaces.ValidAgent() && roomState.Present(promotion.Replaces) && available[promotion.Replaces] {
				if _, unavailable := coreStatus.Availability[promotion.Replaces]; !unavailable && coreStatus.Policy.Restore != chat.CoreRestoreAuto {
					role += ", restoration pending"
				}
			}
		}
		if _, unavailable := coreStatus.Availability[participant]; unavailable {
			state = "unavailable"
		}
		if roomState.Moderator == participant {
			role += ", moderator"
		}
		lines = append(lines, fmt.Sprintf("%-14s %-24s %s", m.plainParticipantLabel(participant), role, state))
	}
	lines = append(lines, "Use /join @agent or /leave @agent. Configure auxiliary identities with /workers and hand any present idle room AI read-only work with /delegate @agent TASK. Returning agents retain their saved session and catch up on missed room messages.")
	m.addNotice(strings.Join(lines, "\n"))
}

func (m *Model) showWorkers() {
	counts := m.orchestrator.WorkerCounts()
	lines := []string{workerCountsSummary(counts), "Configured auxiliary identities:"}
	found := false
	for _, provider := range chat.Agents() {
		for index := 1; index <= counts[provider]; index++ {
			participant, ok := chat.AuxiliaryParticipant(provider, index)
			if !ok {
				continue
			}
			found = true
			lines = append(lines, "  @"+string(participant))
		}
	}
	if !found {
		lines = append(lines, "  none")
	}
	lines = append(lines,
		"Set counts atomically: /workers @codex 2 @claude 1",
		"Set every provider: /workers @all N (subject to the total cap)",
		"Remove every helper: /workers off",
		"A topology change saves the personal setting and reloads this room.")
	m.addNotice(strings.Join(lines, "\n"))
}

func containsCoreParticipant(values []chat.Participant, participant chat.Participant) bool {
	for _, value := range values {
		if value == participant {
			return true
		}
	}
	return false
}

func (m Model) currentSettings() map[chat.Participant]chat.AgentSettings {
	if m.orchestrator != nil {
		return m.orchestrator.EffectiveSettings()
	}
	return map[chat.Participant]chat.AgentSettings{
		chat.Codex:   {Permissions: chat.PermissionWorkspace},
		chat.Claude:  {Permissions: chat.PermissionWorkspace},
		chat.Agy:     {Permissions: chat.PermissionReadOnly},
		chat.Copilot: {Permissions: chat.PermissionReadOnly},
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
	if participant.Provider() == chat.Claude {
		lines = append(lines, "Full provider model IDs are also accepted.")
	}
	return strings.Join(lines, "\n")
}

func loadVoices(controller speech.Controller, filter string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		voices, err := controller.ListVoices(ctx, filter)
		return voicesMsg{filter: filter, voices: voices, err: err}
	}
}

func formatVoices(filter string, voices []speech.Voice) string {
	label := "Speech voices"
	if strings.TrimSpace(filter) != "" {
		label += " matching " + fmt.Sprintf("%q", filter)
	}
	if len(voices) == 0 {
		return label + ": none found"
	}
	const limit = 40
	shown := min(limit, len(voices))
	lines := []string{fmt.Sprintf("%s (%d):", label, len(voices))}
	for _, voice := range voices[:shown] {
		description := strings.TrimSpace(voice.Description)
		if description != "" {
			lines = append(lines, voice.Name+" — "+description)
		} else {
			lines = append(lines, voice.Name)
		}
	}
	if len(voices) > shown {
		lines = append(lines, fmt.Sprintf("… %d more; refine /voices FILTER", len(voices)-shown))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) handleSpeak(fields []string) {
	if m.speech == nil {
		m.addNotice(errorStyle.Render("speech service is unavailable"))
		return
	}
	if len(fields) == 1 {
		m.speechState = m.speech.Snapshot()
		m.addNotice(speechStatus(m.speechState))
		return
	}
	if len(fields) != 2 {
		m.addNotice(errorStyle.Render("usage: /speak [on|off|all|@agent|stop|skip]"))
		return
	}
	value := strings.ToLower(strings.TrimSpace(fields[1]))
	switch value {
	case "on":
		m.setSpeechEnabled(true)
	case "off":
		m.setSpeechEnabled(false)
	case "all":
		m.applySpeechSelection(speech.ModeAll, "")
	case "stop":
		m.speech.Stop()
		m.speechState = m.speech.Snapshot()
		m.status = "speech stopped; queue cleared"
	case "skip":
		m.speech.Skip()
		m.speechState = m.speech.Snapshot()
		m.status = "current speech skipped"
	default:
		participant, ok := chat.ParseParticipant(strings.TrimPrefix(value, "@"))
		if !ok {
			m.addNotice(errorStyle.Render("usage: /speak [on|off|all|@agent|stop|skip]"))
			return
		}
		m.applySpeechSelection(speech.ModeAgent, participant)
	}
}

func (m *Model) handleVoice(fields []string) {
	if m.speech == nil {
		m.addNotice(errorStyle.Render("speech service is unavailable"))
		return
	}
	if len(fields) < 2 {
		m.addNotice(errorStyle.Render("usage: /voice @agent [VOICE_NAME|off]"))
		return
	}
	participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(fields[1], "@")))
	if !ok || !strings.HasPrefix(fields[1], "@") {
		m.addNotice(errorStyle.Render("usage: /voice @agent [VOICE_NAME|off]"))
		return
	}
	if len(fields) == 2 {
		state := m.speech.Snapshot()
		voice := state.Config.Voices[participant]
		if voice == "" {
			voice = "not configured"
		}
		m.addNotice(fmt.Sprintf("%s speech voice: %s", strings.ToUpper(string(participant)), voice))
		return
	}
	voice := strings.TrimSpace(strings.Join(fields[2:], " "))
	if strings.EqualFold(voice, "off") {
		voice = ""
	}
	if err := m.speech.SetVoice(participant, voice); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.speechState = m.speech.Snapshot()
	if voice == "" {
		m.addNotice(fmt.Sprintf("Speech voice cleared for %s", participant))
	} else {
		m.addNotice(fmt.Sprintf("Speech voice for %s set to %s", participant, voice))
	}
	m.status = "speech voice updated"
}

func (m *Model) toggleSpeech() {
	if m.speech == nil {
		m.addNotice(errorStyle.Render("speech service is unavailable"))
		return
	}
	state := m.speech.Snapshot()
	m.setSpeechEnabled(!state.Config.Enabled)
}

func (m *Model) setSpeechEnabled(enabled bool) {
	err := m.speech.SetEnabled(enabled)
	m.speechState = m.speech.Snapshot()
	if err != nil {
		m.addNotice(errorStyle.Render("Speech: " + err.Error()))
		m.status = "speech unavailable"
		return
	}
	if enabled {
		m.status = "speech enabled"
	} else {
		m.status = "speech disabled; queue cleared"
	}
}

func (m *Model) applySpeechSelection(mode speech.Mode, participant chat.Participant) {
	err := m.speech.SetSelection(mode, participant)
	m.speechState = m.speech.Snapshot()
	if err != nil {
		m.addNotice(errorStyle.Render("Speech: " + err.Error()))
		m.status = "speech unavailable"
		return
	}
	if mode == speech.ModeAll {
		m.status = "speech enabled for all configured agents"
	} else {
		m.status = "speech enabled for " + string(participant)
	}
}

func speechStatus(state speech.State) string {
	config := state.Config.WithDefaults()
	selection := "all configured agents"
	if config.Mode == speech.ModeAgent {
		selection = string(config.Agent)
	}
	status := "off"
	if config.Enabled {
		status = "on"
	}
	lines := []string{fmt.Sprintf("Speech: %s; provider: %s; selection: %s; queue: %d", status, config.Provider, selection, state.Queued)}
	if config.Enabled && !state.Available {
		lines[0] += "; unavailable: " + state.Unavailable
	}
	for _, participant := range chat.Agents() {
		voice := config.Voices[participant]
		if voice == "" {
			voice = "not configured"
		}
		lines = append(lines, fmt.Sprintf("  %-7s %s", participant, voice))
	}
	return strings.Join(lines, "\n")
}

func (m Model) speechBadge() string {
	state := m.speechState
	config := state.Config.WithDefaults()
	if !config.Enabled {
		return dimStyle.Render("🔇 OFF")
	}
	if !state.Available {
		return errorStyle.Bold(true).Render("⚠ VOICE UNAVAILABLE")
	}
	selection := "ALL"
	if config.Mode == speech.ModeAgent {
		selection = strings.ToUpper(string(config.Agent))
	}
	label := "🔊 " + selection
	if state.Speaking {
		label += " · " + strings.ToUpper(string(state.CurrentAgent))
	}
	if state.Queued > 0 {
		label += fmt.Sprintf(" · %d queued", state.Queued)
	}
	return busyStyle.Render(label)
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
	m.streamMode = m.room.StreamMode.WithDefault()
	m.turns = cloneTurnRecords(m.room.TurnHistory)
	m.syncRosterActivity()
	m.refreshContent()
}

func (m *Model) syncRoomMetadata() {
	m.room, _ = m.orchestrator.Snapshot()
	m.streamMode = m.room.StreamMode.WithDefault()
	m.turns = cloneTurnRecords(m.room.TurnHistory)
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

func displayModerator(value chat.Participant) string {
	if value == "" {
		return "none"
	}
	return string(value)
}

func (m Model) participantLabel(participant chat.Participant, width int) string {
	name := strings.ToUpper(string(participant))
	if width > 0 {
		name = fmt.Sprintf("%-*s", width, name)
	}
	label := authorStyle(participant).Render(name)
	if participant.ValidAgent() && m.room.Moderator == participant {
		label += " " + moderatorStyle.Render("◆ MOD")
	}
	return label
}

func (m Model) plainParticipantLabel(participant chat.Participant) string {
	label := strings.ToUpper(string(participant))
	if participant.ValidAgent() && m.room.Moderator == participant {
		label += " ◆ MOD"
	}
	return label
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
	if author == chat.User {
		return userStyle
	}
	switch author.Provider() {
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
