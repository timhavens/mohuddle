package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
)

type Store interface {
	SaveRoom(chat.Room) error
	AppendMessage(string, chat.Message) error
}

type Preferences interface {
	Default(chat.Participant) chat.AgentSettings
	Effective(chat.Room, chat.Participant) chat.AgentSettings
	SetDefault(chat.Participant, chat.AgentSettings) error
	FullAccessAcknowledged() bool
	AcknowledgeFullAccess() error
	DetailsVisible() bool
	SetDetailsVisible(bool) error
}

type EventType string

const (
	EventMessage      EventType = "message"
	EventAgent        EventType = "agent"
	EventWaveStarted  EventType = "wave_started"
	EventTurnStarted  EventType = "turn_started"
	EventTurnFinished EventType = "turn_finished"
	EventRoundDone    EventType = "round_done"
	EventConflict     EventType = "conflict"
	EventError        EventType = "error"
)

type Event struct {
	Type         EventType
	Participant  chat.Participant
	Participants []chat.Participant
	Wave         int
	Message      *chat.Message
	AgentEvent   *agent.Event
	Err          error
	Text         string
}

type activeTurn struct {
	version uint64
	cancel  context.CancelFunc
}

type turnSpec struct {
	after       uint64
	through     uint64
	readOnly    bool
	ephemeral   bool
	private     bool
	instruction string
}

type turnOutcome struct {
	participant chat.Participant
	result      agent.TurnResult
	ran         bool
	failed      bool
	canceled    bool
}

type Orchestrator struct {
	store       Store
	preferences Preferences
	agents      map[chat.Participant]agent.Agent
	settings    map[chat.Participant]chat.AgentSettings
	launch      map[chat.Participant]chat.AgentSettings

	mu           sync.Mutex
	persistMu    sync.Mutex
	room         chat.Room
	messages     []chat.Message
	nextSequence uint64
	activeWork   int
	version      uint64
	activeTurns  map[chat.Participant]activeTurn
	closed       bool

	agentGates map[chat.Participant]*sync.Mutex
	events     chan Event
	lifetime   context.Context
	stop       context.CancelFunc
	wg         sync.WaitGroup
}

func New(room chat.Room, messages []chat.Message, roomStore Store, agents ...agent.Agent) (*Orchestrator, error) {
	if roomStore == nil {
		return nil, fmt.Errorf("room store is required")
	}
	agentMap := make(map[chat.Participant]agent.Agent, len(agents))
	for _, value := range agents {
		if value == nil || !value.Participant().ValidAgent() {
			return nil, fmt.Errorf("invalid agent")
		}
		agentMap[value.Participant()] = value
	}
	if len(agentMap) == 0 {
		return nil, fmt.Errorf("at least one agent is required")
	}
	if room.MaxWaves < 1 {
		room.MaxWaves = 3
	}
	if room.Members == nil {
		room.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true}
	}
	if room.Sessions == nil {
		room.Sessions = make(map[chat.Participant]chat.AgentSession, len(agentMap))
	}
	if room.Settings == nil {
		room.Settings = make(map[chat.Participant]chat.AgentSettings, len(agentMap))
	}
	for _, participant := range []chat.Participant{chat.Agy, chat.Copilot} {
		if value, ok := room.Settings[participant]; ok {
			value.Permissions = chat.PermissionReadOnly
			room.Settings[participant] = value
		}
	}
	grants := room.Grants[:0]
	for _, grant := range room.Grants {
		if !grant.Participant.VoiceOnly() {
			grants = append(grants, grant)
		}
	}
	room.Grants = grants
	for participant := range agentMap {
		if _, ok := room.Sessions[participant]; !ok {
			room.Sessions[participant] = chat.AgentSession{}
		}
	}
	for _, participant := range []chat.Participant{chat.Agy, chat.Copilot} {
		// Demotion discards any native worker-session context. Voice turns are
		// disposable and reconstruct their context from the public transcript.
		room.Sessions[participant] = chat.AgentSession{}
	}
	for _, participant := range room.PresentAgents() {
		if agentMap[participant] == nil {
			return nil, fmt.Errorf("room member %s is unavailable", participant)
		}
	}
	if !room.Moderator.CoreWorker() || !room.Present(room.Moderator) || agentMap[room.Moderator] == nil {
		room.Moderator = firstPresentCore(room, agentMap)
	}
	ctx, cancel := context.WithCancel(context.Background())
	orchestrator := &Orchestrator{
		store:       roomStore,
		agents:      agentMap,
		room:        room,
		messages:    append([]chat.Message(nil), messages...),
		settings:    make(map[chat.Participant]chat.AgentSettings, len(agentMap)),
		activeTurns: make(map[chat.Participant]activeTurn, len(agentMap)),
		agentGates:  make(map[chat.Participant]*sync.Mutex, len(agentMap)),
		events:      make(chan Event, 512),
		lifetime:    ctx,
		stop:        cancel,
	}
	for participant := range agentMap {
		permissions := chat.PermissionWorkspace
		if participant.VoiceOnly() {
			permissions = chat.PermissionReadOnly
		}
		orchestrator.settings[participant] = chat.AgentSettings{Permissions: permissions}
		orchestrator.agentGates[participant] = &sync.Mutex{}
	}
	for _, message := range messages {
		if message.Sequence >= orchestrator.nextSequence {
			orchestrator.nextSequence = message.Sequence + 1
		}
	}
	if orchestrator.nextSequence == 0 {
		orchestrator.nextSequence = 1
	}
	return orchestrator, nil
}

func (o *Orchestrator) Events() <-chan Event { return o.events }

func (o *Orchestrator) Snapshot() (chat.Room, []chat.Message) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return cloneRoom(o.room), append([]chat.Message(nil), o.messages...)
}

func cloneRoom(value chat.Room) chat.Room {
	value.Members = cloneMap(value.Members)
	value.Sessions = cloneMap(value.Sessions)
	value.Settings = cloneMap(value.Settings)
	value.Grants = append([]chat.AccessGrant(nil), value.Grants...)
	if value.Conflict != nil {
		conflict := *value.Conflict
		conflict.Reasons = cloneMap(value.Conflict.Reasons)
		value.Conflict = &conflict
	}
	return value
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// saveRoom takes a fresh snapshot only after acquiring the persistence lock.
// That prevents a slower concurrent turn from overwriting newer session state.
func (o *Orchestrator) saveRoom() error {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	roomCopy := cloneRoom(o.room)
	o.mu.Unlock()
	return o.store.SaveRoom(roomCopy)
}

func (o *Orchestrator) participantsLocked() []chat.Participant {
	result := make([]chat.Participant, 0, len(o.agents))
	for _, participant := range chat.Agents() {
		if o.agents[participant] != nil {
			result = append(result, participant)
		}
	}
	return result
}

func (o *Orchestrator) Participants() []chat.Participant {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.participantsLocked()
}

func (o *Orchestrator) PresentAgents() []chat.Participant {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]chat.Participant(nil), o.room.PresentAgents()...)
}

func firstPresentCore(room chat.Room, agents map[chat.Participant]agent.Agent) chat.Participant {
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		if room.Present(participant) && agents[participant] != nil {
			return participant
		}
	}
	return ""
}

func (o *Orchestrator) Moderator() chat.Participant {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.room.Moderator
}

func (o *Orchestrator) SetModerator(participant chat.Participant) error {
	if !participant.CoreWorker() {
		return fmt.Errorf("moderator must be @codex or @claude")
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing the moderator")
	}
	if o.agents[participant] == nil || !o.room.Present(participant) {
		o.mu.Unlock()
		return fmt.Errorf("%s must be present and available to moderate", participant)
	}
	if o.room.Moderator == participant {
		o.mu.Unlock()
		return nil
	}
	o.room.Moderator = participant
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	_, err := o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("%s is now the moderator", participant))
	return err
}

func (o *Orchestrator) SetPresence(participant chat.Participant, present bool) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing the room roster")
	}
	if o.agents[participant] == nil {
		o.mu.Unlock()
		return fmt.Errorf("%s is not available in this MoHuddle process", participant)
	}
	if o.room.Members == nil {
		o.room.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true}
	}
	if o.room.Members[participant] == present {
		o.mu.Unlock()
		return nil
	}
	if present {
		o.room.Members[participant] = true
	} else {
		delete(o.room.Members, participant)
	}
	moderatorChanged := false
	if !present && o.room.Moderator == participant {
		o.room.Moderator = firstPresentCore(o.room, o.agents)
		moderatorChanged = true
	} else if present && o.room.Moderator == "" && participant.CoreWorker() {
		o.room.Moderator = participant
		moderatorChanged = true
	}
	if o.room.Sessions == nil {
		o.room.Sessions = make(map[chat.Participant]chat.AgentSession)
	}
	if _, ok := o.room.Sessions[participant]; !ok {
		o.room.Sessions[participant] = chat.AgentSession{}
	}
	newModerator := o.room.Moderator
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	action := "left the room"
	if present {
		action = "joined the room"
	}
	_, err := o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("%s %s", participant, action))
	if err == nil && moderatorChanged && newModerator.ValidAgent() {
		_, err = o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("%s is now the moderator", newModerator))
	}
	return err
}

func (o *Orchestrator) Configure(preferences Preferences, launch map[chat.Participant]chat.AgentSettings) error {
	o.mu.Lock()
	o.preferences = preferences
	o.launch = make(map[chat.Participant]chat.AgentSettings, len(launch))
	for participant, value := range launch {
		o.launch[participant] = value
	}
	for _, participant := range o.participantsLocked() {
		value := chat.AgentSettings{Permissions: chat.PermissionWorkspace}
		if preferences != nil {
			value = preferences.Effective(o.room, participant)
		}
		o.applySettingsLocked(participant, effectiveRoleSettings(participant, mergeSettings(value, o.launch[participant])))
	}
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) EffectiveSettings() map[chat.Participant]chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make(map[chat.Participant]chat.AgentSettings, len(chat.Agents()))
	for _, participant := range chat.Agents() {
		value, ok := o.settings[participant]
		if !ok {
			value = chat.AgentSettings{Permissions: chat.PermissionWorkspace}
			if o.preferences != nil {
				value = o.preferences.Effective(o.room, participant)
			}
			value = mergeSettings(value, o.launch[participant])
		}
		result[participant] = effectiveRoleSettings(participant, value)
	}
	return result
}

func (o *Orchestrator) DefaultSettings() map[chat.Participant]chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := map[chat.Participant]chat.AgentSettings{}
	for _, participant := range chat.Agents() {
		value := chat.AgentSettings{Permissions: chat.PermissionWorkspace}
		if o.preferences != nil {
			value = o.preferences.Default(participant)
		}
		result[participant] = effectiveRoleSettings(participant, value)
	}
	return result
}

func (o *Orchestrator) RoomSettings() map[chat.Participant]chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make(map[chat.Participant]chat.AgentSettings, len(chat.Agents()))
	for _, participant := range chat.Agents() {
		value, ok := o.room.Settings[participant]
		if !ok {
			value = chat.AgentSettings{Permissions: chat.PermissionWorkspace}
			if o.preferences != nil {
				value = o.preferences.Default(participant)
			}
		}
		result[participant] = effectiveRoleSettings(participant, value)
	}
	return result
}

func (o *Orchestrator) FullAccessAcknowledged() bool {
	o.mu.Lock()
	preferences := o.preferences
	o.mu.Unlock()
	return preferences != nil && preferences.FullAccessAcknowledged()
}

func (o *Orchestrator) DetailsVisible() bool {
	o.mu.Lock()
	preferences := o.preferences
	o.mu.Unlock()
	return preferences != nil && preferences.DetailsVisible()
}

func (o *Orchestrator) SetDetailsVisible(visible bool) error {
	o.mu.Lock()
	preferences := o.preferences
	o.mu.Unlock()
	if preferences == nil {
		return fmt.Errorf("personal settings are unavailable")
	}
	return preferences.SetDetailsVisible(visible)
}

func (o *Orchestrator) Models(ctx context.Context, participant chat.Participant) ([]agent.ModelOption, error) {
	if !participant.ValidAgent() {
		return nil, fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return nil, fmt.Errorf("wait for active work to finish before loading models")
	}
	catalog, ok := o.agents[participant].(agent.ModelCatalog)
	o.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s does not provide a model catalog", participant)
	}
	return catalog.Models(ctx)
}

func (o *Orchestrator) AcknowledgeFullAccess() error {
	o.mu.Lock()
	preferences := o.preferences
	o.mu.Unlock()
	if preferences == nil {
		return fmt.Errorf("personal settings are unavailable")
	}
	return preferences.AcknowledgeFullAccess()
}

func (o *Orchestrator) SetAgentSettings(participant chat.Participant, value chat.AgentSettings, personalDefault bool) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	value = appsettings.Normalize(value)
	value = effectiveRoleSettings(participant, value)
	if err := appsettings.ValidateFor(participant, value); err != nil {
		return err
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing settings")
	}
	if value.Permissions == chat.PermissionFull && (o.preferences == nil || !o.preferences.FullAccessAcknowledged()) {
		o.mu.Unlock()
		return fmt.Errorf("full access requires acknowledgement")
	}
	if personalDefault {
		if o.preferences == nil {
			o.mu.Unlock()
			return fmt.Errorf("personal settings are unavailable")
		}
		if err := o.preferences.SetDefault(participant, value); err != nil {
			o.mu.Unlock()
			return err
		}
		if _, overridden := o.room.Settings[participant]; overridden {
			o.mu.Unlock()
			return nil
		}
	} else {
		if o.room.Settings == nil {
			o.room.Settings = make(map[chat.Participant]chat.AgentSettings, len(chat.Agents()))
		}
		o.room.Settings[participant] = value
	}
	o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) InheritAgentSettings(participant chat.Participant) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing settings")
	}
	delete(o.room.Settings, participant)
	value := chat.AgentSettings{Permissions: chat.PermissionWorkspace}
	if o.preferences != nil {
		value = o.preferences.Default(participant)
	}
	o.applySettingsLocked(participant, effectiveRoleSettings(participant, mergeSettings(value, o.launch[participant])))
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) applySettingsLocked(participant chat.Participant, value chat.AgentSettings) {
	value = effectiveRoleSettings(participant, value)
	o.settings[participant] = value
	if configurable, ok := o.agents[participant].(agent.Configurable); ok && configurable.Configure(value) {
		o.room.Sessions[participant] = chat.AgentSession{}
	}
}

func effectiveRoleSettings(participant chat.Participant, value chat.AgentSettings) chat.AgentSettings {
	value = value.WithDefaults()
	if participant.VoiceOnly() {
		value.Permissions = chat.PermissionReadOnly
	}
	return value
}

func mergeSettings(base, override chat.AgentSettings) chat.AgentSettings {
	if override.Model != "" {
		base.Model = override.Model
	}
	if override.Effort != "" {
		base.Effort = override.Effort
	}
	if override.Permissions.Valid() {
		base.Permissions = override.Permissions
	}
	return base.WithDefaults()
}

func (o *Orchestrator) Post(text string) error {
	target, publicText := parseTarget(text)
	if strings.TrimSpace(publicText) == "" {
		return fmt.Errorf("message is empty")
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	if target.ValidAgent() {
		if o.agents[target] == nil {
			o.mu.Unlock()
			return fmt.Errorf("%s is unavailable", target)
		}
		if !o.room.Present(target) {
			o.mu.Unlock()
			return fmt.Errorf("%s is away; use /join @%s first", target, target)
		}
	} else if o.room.Moderator == "" {
		o.mu.Unlock()
		return fmt.Errorf("an untagged message needs Codex or Claude in the room")
	}
	o.room.Conflict = nil
	o.mu.Unlock()

	message, err := o.appendMessage(chat.User, target, chat.MessageText, publicText)
	if err != nil {
		return err
	}
	if err := o.saveRoom(); err != nil {
		return err
	}

	o.mu.Lock()
	version, moderator, present, err := o.beginWorkflowLocked()
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if target.ValidAgent() {
		go o.runDirectWorkflow(message.Sequence, target, version)
	} else {
		go o.runModeratedWorkflow(message.Sequence, moderator, present, version)
	}
	return nil
}

// Ask runs one explicit concurrent response from each selected present agent.
// Leading @agent tokens select the participants; without them all present
// agents participate.
func (o *Orchestrator) Ask(text string) error {
	selected, publicText, err := parseAsk(text)
	if err != nil {
		return err
	}
	if strings.TrimSpace(publicText) == "" {
		return fmt.Errorf("message is empty")
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	if len(selected) == 0 {
		selected = append([]chat.Participant(nil), o.room.PresentAgents()...)
	}
	if len(selected) == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no agents are in the room; use /join @agent")
	}
	for _, participant := range selected {
		if o.agents[participant] == nil || !o.room.Present(participant) {
			o.mu.Unlock()
			return fmt.Errorf("%s is not present and available", participant)
		}
	}
	o.room.Conflict = nil
	o.mu.Unlock()

	message, err := o.appendMessage(chat.User, "", chat.MessageText, publicText)
	if err != nil {
		return err
	}
	if err := o.saveRoom(); err != nil {
		return err
	}

	o.mu.Lock()
	version, _, _, err := o.beginWorkflowLocked()
	o.mu.Unlock()
	if err != nil {
		return err
	}
	go o.runOneShotWorkflow(message.Sequence, selected, version)
	return nil
}

func (o *Orchestrator) beginWorkflowLocked() (uint64, chat.Participant, []chat.Participant, error) {
	if o.closed {
		return 0, "", nil, fmt.Errorf("room is closed")
	}
	o.cancelAllLocked()
	o.version++
	o.activeWork++
	o.wg.Add(1)
	return o.version, o.room.Moderator, append([]chat.Participant(nil), o.room.PresentAgents()...), nil
}

func (o *Orchestrator) Continue() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	if len(o.messages) == 0 {
		o.mu.Unlock()
		return fmt.Errorf("there is no conversation to continue")
	}
	moderator := o.room.Moderator
	participants := append([]chat.Participant(nil), o.room.PresentAgents()...)
	if moderator == "" {
		o.mu.Unlock()
		return fmt.Errorf("continuing an untagged round needs Codex or Claude in the room")
	}
	after := o.messages[len(o.messages)-1].Sequence
	o.room.Conflict = nil
	o.cancelAllLocked()
	o.version++
	version := o.version
	o.activeWork++
	o.wg.Add(1)
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		o.finishWorkflow()
		o.wg.Done()
		return err
	}
	go o.runModeratedWorkflow(after, moderator, participants, version)
	return nil
}

func (o *Orchestrator) Stop() {
	o.mu.Lock()
	o.version++
	o.cancelAllLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) cancelAllLocked() {
	for _, turn := range o.activeTurns {
		turn.cancel()
	}
}

func (o *Orchestrator) runDirectWorkflow(after uint64, participant chat.Participant, version uint64) {
	defer o.wg.Done()
	defer func() {
		o.finishWorkflow()
	}()
	through := o.latestSequence()
	instruction := "Answer the human directly. This is a one-agent turn: do not request or wait for peer review."
	if participant.VoiceOnly() {
		instruction = voiceInstruction + " Answer the human directly from the transcript only."
	}
	outcome := o.runOne(participant, version, turnSpec{after: after, through: through, instruction: instruction})
	if !o.workflowCurrent(version) {
		return
	}
	text := fmt.Sprintf("%s completed the direct turn", participant)
	if outcome.canceled {
		text = "Direct turn paused after the agent was canceled"
	} else if outcome.failed || !outcome.ran {
		text = "Direct turn paused after an agent error"
	}
	o.send(Event{Type: EventRoundDone, Text: text})
}

func (o *Orchestrator) runOneShotWorkflow(after uint64, participants []chat.Participant, version uint64) {
	defer o.wg.Done()
	defer o.finishWorkflow()
	through := o.latestSequence()
	outcomes := o.runWave(participants, version, 1, "explicit one-shot", turnSpec{
		after: after, through: through, readOnly: true,
		instruction: "Answer the human independently. Address only the human; do not review peers. This is your only response.",
	})
	if !o.workflowCurrent(version) {
		return
	}
	text := "Selected agents completed the one-shot"
	if waveFailed(outcomes) {
		text = "One-shot paused after an agent error or cancellation"
	}
	o.send(Event{Type: EventRoundDone, Wave: 1, Text: text})
}

type leadBid struct {
	Participant   chat.Participant `json:"participant"`
	PreferredLead chat.Participant `json:"preferred_lead"`
	Fit           string           `json:"fit"`
	Reason        string           `json:"reason"`
}

const voiceInstruction = "You are a voice-only participant. Use only the supplied room transcript. You have no tools or workspace access. Never request access, suggest that greater access would improve your answer, offer to perform repository work, list missing capabilities, or recommend changing your role. Speak only when you have a distinct, relevant contribution."

func (o *Orchestrator) runModeratedWorkflow(after uint64, moderator chat.Participant, present []chat.Participant, version uint64) {
	defer o.wg.Done()
	defer o.finishWorkflow()
	through := o.latestSequence()
	bids := o.runLeadBids(after, through, present, version)
	if !o.workflowCurrent(version) {
		return
	}
	invited := make(map[chat.Participant]bool)
	current := moderator
	floorAfter := after
	firstModeratorTurn := true
	for {
		through = o.latestSequence()
		instruction := "You were invited by the moderator. Address the current request. Your turn returns to the moderator automatically; do not route another participant."
		if current == moderator {
			instruction = moderatorInstruction(moderator, present, invited, bids, firstModeratorTurn)
			firstModeratorTurn = false
		} else if current.VoiceOnly() {
			instruction = voiceInstruction + " You were invited by the moderator; after your response the floor returns to the moderator."
		}
		outcome := o.runOne(current, version, turnSpec{after: floorAfter, through: through, instruction: instruction})
		if !o.workflowCurrent(version) {
			return
		}
		if outcome.failed || !outcome.ran {
			action := "failed"
			if outcome.canceled {
				action = "was canceled"
			}
			o.send(Event{Type: EventRoundDone, Text: fmt.Sprintf("Moderated round paused after %s %s", current, action)})
			return
		}
		floorAfter = through
		if current != moderator {
			current = moderator
			continue
		}
		next := outcome.result.Next
		if next == "" {
			if outcome.result.Disagrees || !outcome.result.Done {
				conflict := o.setConflict(0, []turnOutcome{outcome})
				o.send(Event{Type: EventConflict, Participant: conflict.RaisedBy, Text: "The moderator paused for your direction"})
			} else {
				o.clearConflict()
				o.send(Event{Type: EventRoundDone, Text: fmt.Sprintf("%s ended the moderated round", moderator)})
			}
			return
		}
		if next == moderator || !containsParticipant(present, next) || invited[next] {
			o.send(Event{Type: EventError, Participant: moderator, Err: fmt.Errorf("moderator selected unavailable or already-invited participant %s", next)})
			o.send(Event{Type: EventRoundDone, Text: "Moderated round paused after an invalid floor decision"})
			return
		}
		invited[next] = true
		current = next
	}
}

func (o *Orchestrator) runLeadBids(after, through uint64, present []chat.Participant, version uint64) []leadBid {
	participants := make([]chat.Participant, 0, 2)
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		if containsParticipant(present, participant) {
			participants = append(participants, participant)
		}
	}
	bids := make([]leadBid, len(participants))
	var wait sync.WaitGroup
	wait.Add(len(participants))
	for index, participant := range participants {
		go func(index int, participant chat.Participant) {
			defer wait.Done()
			instruction := fmt.Sprintf("Private routing bid. Do not perform the task or use tools. Decide whether Codex or Claude is the better lead for the current human request. Return only JSON: {\"participant\":%q,\"preferred_lead\":\"codex|claude\",\"fit\":\"high|medium|low\",\"reason\":\"short reason\"}, followed by the required private control marker.", participant)
			outcome := o.runOne(participant, version, turnSpec{after: 0, through: through, readOnly: true, ephemeral: true, private: true, instruction: instruction})
			bid := leadBid{Participant: participant, PreferredLead: participant, Fit: "unknown", Reason: "bid unavailable"}
			if outcome.ran && !outcome.failed && json.Unmarshal([]byte(outcome.result.Text), &bid) == nil {
				bid.Participant = participant
				if !bid.PreferredLead.CoreWorker() || !containsParticipant(participants, bid.PreferredLead) {
					bid.PreferredLead = participant
				}
				bid.Reason = strings.TrimSpace(bid.Reason)
			}
			bids[index] = bid
		}(index, participant)
	}
	wait.Wait()
	return bids
}

func moderatorInstruction(moderator chat.Participant, present []chat.Participant, invited map[chat.Participant]bool, bids []leadBid, initial bool) string {
	encoded, _ := json.Marshal(bids)
	phase := "A participant has just returned the floor. End silently if the answer is sufficient; otherwise correct, synthesize, or invite one remaining participant."
	if initial {
		phase = "Use the private Codex/Claude bids to choose the task lead. If you are the best lead, answer or perform the authorized work in this turn. Otherwise delegate silently."
	}
	available := make([]string, 0)
	for _, participant := range present {
		if participant != moderator && !invited[participant] {
			available = append(available, string(participant))
		}
	}
	return fmt.Sprintf("You are the room moderator; you never moderate the human. %s Private bids: %s. Remaining participants you may invite once: %s. To give one of them the floor, set next in your private mohuddle marker and set done:false. To end, omit next and set done:true. Do not repeat or summarize a sufficient response, and invite AGY or Copilot only for a materially distinct perspective.", phase, encoded, strings.Join(available, ", "))
}

func containsParticipant(values []chat.Participant, participant chat.Participant) bool {
	for _, value := range values {
		if value == participant {
			return true
		}
	}
	return false
}

func (o *Orchestrator) runWave(participants []chat.Participant, version uint64, wave int, text string, spec turnSpec) []turnOutcome {
	if len(participants) == 0 {
		return nil
	}
	o.send(Event{Type: EventWaveStarted, Participants: append([]chat.Participant(nil), participants...), Wave: wave, Text: text})
	outcomes := make([]turnOutcome, len(participants))
	var wait sync.WaitGroup
	wait.Add(len(participants))
	for index, participant := range participants {
		go func(index int, participant chat.Participant) {
			defer wait.Done()
			outcomes[index] = o.runOne(participant, version, spec)
		}(index, participant)
	}
	wait.Wait()
	return outcomes
}

func (o *Orchestrator) runOne(participant chat.Participant, version uint64, spec turnSpec) turnOutcome {
	outcome := turnOutcome{participant: participant}
	gate := o.agentGates[participant]
	gate.Lock()
	defer gate.Unlock()
	if !o.workflowCurrent(version) {
		return outcome
	}

	ctx, cancel := context.WithCancel(o.lifetime)
	o.mu.Lock()
	if o.closed || o.version != version {
		o.mu.Unlock()
		cancel()
		return outcome
	}
	o.activeTurns[participant] = activeTurn{version: version, cancel: cancel}
	o.mu.Unlock()
	if !spec.private {
		o.send(Event{Type: EventTurnStarted, Participant: participant})
	}
	finish := func() { o.finishTurnWithVisibility(participant, version, cancel, !spec.private) }

	var draftMu sync.Mutex
	var draft strings.Builder
	emit := o.agentEmitter(ctx, participant, &draftMu, &draft)
	if spec.private {
		emit = func(agent.Event) {}
	}
	request := o.turnRequest(participant, spec, nil)
	result, err := o.agents[participant].Run(ctx, request, emit)
	outcome.ran = true
	if ctx.Err() != nil || !o.workflowCurrent(version) {
		if !spec.private {
			o.appendInterrupted(participant, &draftMu, &draft)
		}
		finish()
		return outcome
	}
	if err != nil {
		if !spec.private {
			o.appendInterrupted(participant, &draftMu, &draft)
		}
		if errors.Is(err, context.Canceled) {
			outcome.failed = true
			outcome.canceled = true
			finish()
			return outcome
		}
		if !spec.private {
			o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s: %w", participant, err)})
		}
		outcome.failed = true
		finish()
		return outcome
	}
	outcome.result = result
	if spec.private {
		finish()
		return outcome
	}
	if participant.VoiceOnly() && result.AccessRequest != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s voice-only turn attempted to request access", participant)})
		outcome.failed = true
		finish()
		return outcome
	}

	lastSequence, err := o.recordResult(participant, result, spec.through)
	if err != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: err})
		outcome.failed = true
		finish()
		return outcome
	}
	draftMu.Lock()
	draft.Reset()
	draftMu.Unlock()

	settings := o.settingsFor(participant)
	if spec.readOnly {
		settings.Permissions = chat.PermissionReadOnly
	}
	if result.AccessRequest != nil && settings.Permissions != chat.PermissionFull {
		grant, temporary, accepted := o.requestAccess(ctx, participant, *result.AccessRequest)
		if accepted && o.workflowCurrent(version) {
			if !temporary {
				o.addGrant(grant)
			}
			retrySpec := spec
			retrySpec.after = lastSequence
			retrySpec.through = o.latestSequence()
			retrySpec.instruction = "Access was approved. Continue the work you paused using the newly granted path, then report the result."
			result, err = o.agents[participant].Run(ctx, o.turnRequest(participant, retrySpec, &grant), emit)
			if ctx.Err() != nil || !o.workflowCurrent(version) {
				o.appendInterrupted(participant, &draftMu, &draft)
				finish()
				return outcome
			}
			if err != nil {
				o.appendInterrupted(participant, &draftMu, &draft)
				if errors.Is(err, context.Canceled) {
					outcome.failed = true
					outcome.canceled = true
					finish()
					return outcome
				}
				o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s retry: %w", participant, err)})
				outcome.failed = true
				finish()
				return outcome
			}
			if _, err := o.recordResult(participant, result, retrySpec.through); err != nil {
				o.send(Event{Type: EventError, Participant: participant, Err: err})
				outcome.failed = true
				finish()
				return outcome
			}
			outcome.result = result
		}
	}
	finish()
	return outcome
}

func (o *Orchestrator) finishTurn(participant chat.Participant, version uint64, cancel context.CancelFunc) {
	o.finishTurnWithVisibility(participant, version, cancel, true)
}

func (o *Orchestrator) finishTurnWithVisibility(participant chat.Participant, version uint64, cancel context.CancelFunc, visible bool) {
	cancel()
	o.mu.Lock()
	if current, ok := o.activeTurns[participant]; ok && current.version == version {
		delete(o.activeTurns, participant)
	}
	o.mu.Unlock()
	if visible {
		o.send(Event{Type: EventTurnFinished, Participant: participant})
	}
}

func (o *Orchestrator) appendInterrupted(participant chat.Participant, draftMu *sync.Mutex, draft *strings.Builder) {
	draftMu.Lock()
	value := draft.String()
	draftMu.Unlock()
	public, _, _ := agent.ParseResponse(value)
	if marker := strings.Index(public, "<!--"); marker >= 0 {
		public = public[:marker]
	}
	public = strings.TrimSpace(public)
	if public != "" {
		if _, err := o.appendMessage(participant, "", chat.MessageInterrupted, public); err != nil {
			o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("save interrupted %s draft: %w", participant, err)})
		}
	}
}

func (o *Orchestrator) agentEmitter(ctx context.Context, participant chat.Participant, draftMu *sync.Mutex, draft *strings.Builder) func(agent.Event) {
	return func(event agent.Event) {
		if ctx.Err() != nil {
			return
		}
		if event.Agent == "" {
			event.Agent = participant
		}
		if event.Type == agent.EventDelta {
			draftMu.Lock()
			draft.WriteString(event.Text)
			draftMu.Unlock()
		}
		if event.Type == agent.EventTool && strings.TrimSpace(event.Text) != "" {
			if _, err := o.appendMessage(event.Agent, "", chat.MessageTool, event.Text); err != nil {
				o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("save %s tool activity: %w", participant, err)})
			}
		}
		o.send(Event{Type: EventAgent, AgentEvent: &event})
	}
}

func (o *Orchestrator) turnRequest(participant chat.Participant, spec turnSpec, temporary *chat.AccessGrant) agent.TurnRequest {
	o.mu.Lock()
	messages := make([]chat.Message, 0)
	cursor := o.room.Sessions[participant].Cursor
	if spec.ephemeral || participant.VoiceOnly() {
		cursor = 0
	}
	for _, message := range o.messages {
		if message.Sequence <= spec.through && (message.Sequence > spec.after || message.Sequence > cursor) {
			messages = append(messages, message)
		}
	}
	roomCopy := cloneRoom(o.room)
	configured := o.settings[participant].WithDefaults()
	o.mu.Unlock()
	if spec.readOnly || participant.VoiceOnly() {
		configured.Permissions = chat.PermissionReadOnly
	}
	if temporary != nil {
		roomCopy.Grants = append(roomCopy.Grants, *temporary)
	}
	systemPrompt := agent.RoomProtocolPromptFor(configured)
	if participant == chat.Claude {
		systemPrompt += "\n\nClaude response style:\nKeep public replies especially concise. Lead with the answer or finding. Do not provide an unsolicited workspace inventory, operating-mode preamble, capability summary, or list of possible next tasks."
	}
	if strings.TrimSpace(spec.instruction) != "" {
		systemPrompt += "\n\nCurrent workflow instruction:\n" + strings.TrimSpace(spec.instruction)
	}
	prompt := transcriptPrompt(messages)
	if spec.private {
		prompt = "HOST-ENFORCED PRIVATE ROUTING: TRANSCRIPT ONLY. You have no workspace or tools during this decision-only bid.\n\n" + prompt
	}
	if participant.VoiceOnly() {
		systemPrompt += "\n\n" + voiceInstruction
		prompt = "HOST-ENFORCED ROLE: VOICE-ONLY. You have no tools, filesystem, repository, network, or access-request capability. Do not suggest changing this role.\n\n" + prompt
	}
	if configured.Permissions == chat.PermissionReadOnly {
		prompt = "HOST-ENFORCED TURN PERMISSIONS: READ-ONLY. You cannot edit files or run mutating actions during this turn. Do not claim that you have write or full access.\n\n" + prompt
	}
	readRoots := access.EffectiveRoots(roomCopy, participant, chat.AccessRead)
	writeRoots := access.EffectiveRoots(roomCopy, participant, chat.AccessReadWrite)
	if participant.VoiceOnly() || spec.private {
		readRoots = nil
		writeRoots = nil
	}
	return agent.TurnRequest{
		Prompt:       prompt,
		Workspace:    roomCopy.Workspace,
		ReadRoots:    readRoots,
		WriteRoots:   writeRoots,
		SystemPrompt: systemPrompt,
		Settings:     configured,
		Ephemeral:    spec.ephemeral,
		NoTools:      spec.private,
		VoiceOnly:    participant.VoiceOnly(),
	}
}

func (o *Orchestrator) settingsFor(participant chat.Participant) chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settings[participant].WithDefaults()
}

func transcriptPrompt(messages []chat.Message) string {
	var value strings.Builder
	value.WriteString("BEGIN UNTRUSTED ROOM TRANSCRIPT\n")
	for _, message := range messages {
		fmt.Fprintf(&value, "[%d] %s", message.Sequence, message.Author)
		if message.Target.ValidAgent() {
			fmt.Fprintf(&value, " -> %s", message.Target)
		}
		fmt.Fprintf(&value, " (%s):\n%s\n\n", message.Kind, message.Text)
	}
	value.WriteString("END UNTRUSTED ROOM TRANSCRIPT\n\nRespond to the room now.")
	return value.String()
}

func (o *Orchestrator) recordResult(participant chat.Participant, result agent.TurnResult, seenThrough uint64) (uint64, error) {
	text := strings.TrimSpace(result.Text)
	if text == "" && result.AccessRequest != nil {
		text = fmt.Sprintf("I need access to %s before I can continue.", result.AccessRequest.Path)
	}
	var sequence uint64
	if text != "" {
		message, err := o.appendMessage(participant, "", chat.MessageText, text)
		if err != nil {
			return 0, err
		}
		sequence = message.Sequence
	}
	o.mu.Lock()
	session := o.room.Sessions[participant]
	session.ID = result.SessionID
	// Cursor means the newest transcript record supplied to the provider, not
	// the sequence of its response. Other participants can post while this turn
	// is running, and those messages must remain eligible for a later floor turn.
	session.Cursor = seenThrough
	o.room.Sessions[participant] = session
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (o *Orchestrator) requestAccess(ctx context.Context, participant chat.Participant, requested agent.AccessRequest) (chat.AccessGrant, bool, bool) {
	o.mu.Lock()
	workspace := o.room.Workspace
	o.mu.Unlock()
	path := requested.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	canonical, err := access.CanonicalDirectory(path)
	if err != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("invalid access request from %s: %w", participant, err)})
		return chat.AccessGrant{}, false, false
	}
	grant := chat.AccessGrant{Path: canonical, Mode: requested.Mode, Participant: participant, CreatedAt: time.Now().UTC()}
	request := &agent.ApprovalRequest{
		Agent: participant, Kind: "directory_access", Title: fmt.Sprintf("Allow %s to access this directory?", participant),
		Description: requested.Reason, Path: canonical, Mode: requested.Mode, Response: make(chan agent.ApprovalDecision, 1),
	}
	event := agent.Event{Type: agent.EventApproval, Agent: participant, Approval: request}
	o.send(Event{Type: EventAgent, AgentEvent: &event})
	select {
	case <-ctx.Done():
		return chat.AccessGrant{}, false, false
	case decision := <-request.Response:
		switch decision {
		case agent.ApproveOnce:
			return grant, true, true
		case agent.ApproveSession:
			return grant, false, true
		case agent.ApproveBoth:
			grant.Participant = chat.System
			return grant, false, true
		default:
			return chat.AccessGrant{}, false, false
		}
	}
}

func (o *Orchestrator) addGrant(grant chat.AccessGrant) {
	o.mu.Lock()
	for i, current := range o.room.Grants {
		if current.Path == grant.Path && current.Participant == grant.Participant {
			if grant.Mode == chat.AccessReadWrite {
				o.room.Grants[i].Mode = grant.Mode
			}
			o.mu.Unlock()
			_ = o.saveRoom()
			return
		}
	}
	o.room.Grants = append(o.room.Grants, grant)
	o.mu.Unlock()
	_ = o.saveRoom()
	_, _ = o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("Granted %s %s access to %s", grant.Participant, grant.Mode, grant.Path))
}

func (o *Orchestrator) RevokeGrant(path string, participant chat.Participant) error {
	canonical, err := access.CanonicalDirectory(path)
	if err != nil {
		return err
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing access grants")
	}
	if canonical == o.room.Workspace {
		o.mu.Unlock()
		return fmt.Errorf("the launch workspace grant cannot be revoked")
	}
	filtered := o.room.Grants[:0]
	for _, grant := range o.room.Grants {
		if grant.Path == canonical && (participant == "" || grant.Participant == participant) {
			continue
		}
		filtered = append(filtered, grant)
	}
	o.room.Grants = filtered
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) appendMessage(author, target chat.Participant, kind chat.MessageKind, text string) (chat.Message, error) {
	o.mu.Lock()
	message := chat.Message{
		Sequence: o.nextSequence, Author: author, Target: target, Kind: kind,
		Text: strings.TrimSpace(text), CreatedAt: time.Now().UTC(),
	}
	id, err := store.NewID()
	if err != nil {
		o.mu.Unlock()
		return chat.Message{}, err
	}
	message.ID = id
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		o.mu.Unlock()
		return chat.Message{}, err
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	o.mu.Unlock()
	o.send(Event{Type: EventMessage, Message: &message})
	return message, nil
}

func (o *Orchestrator) latestSequence() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.messages) == 0 {
		return 0
	}
	return o.messages[len(o.messages)-1].Sequence
}

func (o *Orchestrator) workflowCurrent(version uint64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.closed && o.version == version
}

func (o *Orchestrator) finishWorkflow() {
	o.mu.Lock()
	if o.activeWork > 0 {
		o.activeWork--
	}
	o.mu.Unlock()
}

func waveFailed(outcomes []turnOutcome) bool {
	for _, outcome := range outcomes {
		if outcome.failed || !outcome.ran {
			return true
		}
	}
	return false
}

func (o *Orchestrator) setConflict(wave int, outcomes []turnOutcome) chat.ConflictState {
	reasons := make(map[chat.Participant]string)
	var raisedBy chat.Participant
	for _, outcome := range outcomes {
		if outcome.failed || !outcome.ran {
			continue
		}
		reason := strings.TrimSpace(outcome.result.ConflictReason)
		if outcome.result.Disagrees && reason == "" {
			reason = "reported a material disagreement"
		}
		if !outcome.result.Done && reason == "" {
			reason = "did not mark the workflow complete"
		}
		if reason != "" {
			reasons[outcome.participant] = reason
			if raisedBy == "" {
				raisedBy = outcome.participant
			}
		}
	}
	if raisedBy == "" && len(outcomes) > 0 {
		raisedBy = outcomes[0].participant
		reasons[raisedBy] = "the moderated round paused without completion"
	}
	parts := make([]string, 0, len(reasons))
	for _, participant := range chat.Agents() {
		if reason := reasons[participant]; reason != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", participant, reason))
		}
	}
	conflict := chat.ConflictState{
		RaisedBy:  raisedBy,
		Reason:    strings.Join(parts, "; "),
		Wave:      wave,
		Reasons:   reasons,
		CreatedAt: time.Now().UTC(),
	}
	o.mu.Lock()
	o.room.Conflict = &conflict
	o.mu.Unlock()
	_ = o.saveRoom()
	return conflict
}

func (o *Orchestrator) clearConflict() {
	o.mu.Lock()
	changed := o.room.Conflict != nil
	o.room.Conflict = nil
	o.mu.Unlock()
	if changed {
		_ = o.saveRoom()
	}
}

func (o *Orchestrator) send(event Event) {
	select {
	case o.events <- event:
	case <-o.lifetime.Done():
	}
}

func (o *Orchestrator) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	o.version++
	o.cancelAllLocked()
	o.mu.Unlock()
	o.stop()
	o.wg.Wait()
	participants := o.Participants()
	var errors []string
	for _, participant := range participants {
		if err := o.agents[participant].Close(); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", participant, err))
		}
	}
	close(o.events)
	if len(errors) > 0 {
		return fmt.Errorf("close agents: %s", strings.Join(errors, "; "))
	}
	return nil
}

func parseTarget(value string) (chat.Participant, string) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	for _, participant := range chat.Agents() {
		prefix := "@" + string(participant)
		if lower == prefix {
			return participant, ""
		}
		if strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+"\n") {
			return participant, strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return "", trimmed
}

func parseAsk(value string) ([]chat.Participant, string, error) {
	rest := strings.TrimSpace(value)
	seen := make(map[chat.Participant]bool)
	var participants []chat.Participant
	for strings.HasPrefix(rest, "@") {
		token := rest
		if index := strings.IndexAny(token, " \t\r\n"); index >= 0 {
			token = token[:index]
		}
		participant, ok := chat.ParseParticipant(strings.ToLower(strings.TrimPrefix(token, "@")))
		if !ok {
			return nil, "", fmt.Errorf("unknown /ask participant %s", token)
		}
		if !seen[participant] {
			seen[participant] = true
			participants = append(participants, participant)
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, token))
	}
	return participants, rest, nil
}
