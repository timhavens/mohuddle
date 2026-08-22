package room

import (
	"context"
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
	for participant := range agentMap {
		if _, ok := room.Sessions[participant]; !ok {
			room.Sessions[participant] = chat.AgentSession{}
		}
	}
	for _, participant := range room.PresentAgents() {
		if agentMap[participant] == nil {
			return nil, fmt.Errorf("room member %s is unavailable", participant)
		}
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
		orchestrator.settings[participant] = chat.AgentSettings{Permissions: chat.PermissionWorkspace}
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
	if o.room.Sessions == nil {
		o.room.Sessions = make(map[chat.Participant]chat.AgentSession)
	}
	if _, ok := o.room.Sessions[participant]; !ok {
		o.room.Sessions[participant] = chat.AgentSession{}
	}
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	action := "left the room"
	if present {
		action = "joined the room"
	}
	_, err := o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("%s %s", participant, action))
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
		o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
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
		result[participant] = value.WithDefaults()
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
		result[participant] = value.WithDefaults()
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
		result[participant] = value.WithDefaults()
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
	o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) applySettingsLocked(participant chat.Participant, value chat.AgentSettings) {
	o.settings[participant] = value.WithDefaults()
	if configurable, ok := o.agents[participant].(agent.Configurable); ok && configurable.Configure(value) {
		o.room.Sessions[participant] = chat.AgentSession{}
	}
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
	return o.post(text, false)
}

// Ask broadcasts one independent, read-only turn to every present agent and
// intentionally skips peer-review waves.
func (o *Orchestrator) Ask(text string) error {
	return o.post(text, true)
}

func (o *Orchestrator) post(text string, oneShot bool) error {
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
		if oneShot {
			o.mu.Unlock()
			return fmt.Errorf("/ask is a one-shot broadcast; remove the @agent target")
		}
		if o.agents[target] == nil {
			o.mu.Unlock()
			return fmt.Errorf("%s is unavailable", target)
		}
		if !o.room.Present(target) {
			o.mu.Unlock()
			return fmt.Errorf("%s is away; use /join @%s first", target, target)
		}
	} else if len(o.room.PresentAgents()) == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no agents are in the room; use /join @agent")
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
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	o.cancelAllLocked()
	o.version++
	version := o.version
	o.activeWork++
	o.wg.Add(1)
	if target.ValidAgent() {
		participants := append([]chat.Participant(nil), o.room.PresentAgents()...)
		o.mu.Unlock()
		go o.runTargetWorkflow(message.Sequence, target, participants, version)
		return nil
	}
	participants := append([]chat.Participant(nil), o.room.PresentAgents()...)
	o.mu.Unlock()
	go o.runConsensusWorkflow(message.Sequence, participants, version, oneShot)
	return nil
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
	participants := append([]chat.Participant(nil), o.room.PresentAgents()...)
	if len(participants) == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no agents are in the room; use /join @agent")
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
	go o.runConsensusWorkflow(after, participants, version, false)
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

func (o *Orchestrator) runConsensusWorkflow(after uint64, participants []chat.Participant, version uint64, oneShot bool) {
	defer o.wg.Done()
	var terminal *Event
	defer func() {
		o.finishWorkflow()
		if terminal != nil && o.workflowCurrent(version) {
			o.send(*terminal)
		}
	}()

	maxWaves := o.maxWaves()
	minimumWaves := 1
	if len(participants) > 1 {
		minimumWaves = 2
	}
	for wave := 1; wave <= maxWaves; wave++ {
		through := o.latestSequence()
		instruction := "Independently analyze the human's request. Do not rely on another agent's same-wave work. Report your useful conclusions to the room."
		waveLabel := fmt.Sprintf("consensus wave %d/%d", wave, maxWaves)
		if oneShot {
			instruction = "Answer the human independently. Address only the human; do not review, reply to, or comment on other agents. This is your only response wave."
			waveLabel = "one-shot broadcast"
		}
		if wave > 1 {
			instruction = "Review the other agents' latest public responses. Publish prose only for a material disagreement, correction, missing work, or genuinely new information. If there is nothing substantive to add, return only the private done:true marker and remain publicly silent."
		}
		outcomes := o.runWave(participants, version, wave, waveLabel, turnSpec{
			after: after, through: through, readOnly: true, instruction: instruction,
		})
		if !o.workflowCurrent(version) {
			return
		}
		if waveFailed(outcomes) {
			text := "Workflow paused after an agent error; use /continue when ready"
			if waveCanceled(outcomes) {
				text = "Workflow paused after an agent cancellation; use /continue when ready"
			}
			terminal = &Event{Type: EventRoundDone, Text: text}
			return
		}
		if oneShot {
			if outcomesDisagree(outcomes) {
				conflict := o.setConflict(wave, outcomes)
				terminal = &Event{Type: EventConflict, Participant: conflict.RaisedBy, Wave: wave, Text: "An agent reported a material conflict; your direction is required"}
			} else {
				o.clearConflict()
				terminal = &Event{Type: EventRoundDone, Wave: wave, Text: "All present agents completed the one-shot broadcast"}
			}
			return
		}
		if wave >= minimumWaves && outcomesAgree(outcomes) {
			o.clearConflict()
			terminal = &Event{Type: EventRoundDone, Wave: wave, Text: fmt.Sprintf("All present agents reached consensus in %d wave(s)", wave)}
			return
		}
		if wave == maxWaves {
			conflict := o.setConflict(wave, outcomes)
			terminal = &Event{Type: EventConflict, Participant: conflict.RaisedBy, Wave: wave, Text: "Consensus cap reached; your direction is required"}
			return
		}
		after = through
	}
}

func (o *Orchestrator) runTargetWorkflow(after uint64, editor chat.Participant, present []chat.Participant, version uint64) {
	defer o.wg.Done()
	var terminal *Event
	defer func() {
		o.finishWorkflow()
		if terminal != nil && o.workflowCurrent(version) {
			o.send(*terminal)
		}
	}()

	through := o.latestSequence()
	o.send(Event{Type: EventWaveStarted, Participants: []chat.Participant{editor}, Wave: 1, Text: fmt.Sprintf("%s is the editor", editor)})
	editorOutcome := o.runOne(editor, version, turnSpec{
		after: after, through: through,
		instruction: "You are the selected editor for this targeted turn. Answer the human and perform any authorized work, then report the result publicly.",
	})
	if !o.workflowCurrent(version) {
		return
	}
	if editorOutcome.failed || !editorOutcome.ran {
		text := "Targeted workflow paused after an editor error; send a new message or use /continue"
		if editorOutcome.canceled {
			text = "Targeted workflow paused after the editor was canceled; send a new message or use /continue"
		}
		terminal = &Event{Type: EventRoundDone, Text: text}
		return
	}

	reviewers := withoutParticipant(present, editor)
	if len(reviewers) == 0 {
		terminal = &Event{Type: EventRoundDone, Text: fmt.Sprintf("%s completed the targeted turn; no other agent is present to review it", editor)}
		return
	}

	maxAttempts := o.maxWaves()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		reviewStart := o.latestSequence()
		reviews := o.runWave(reviewers, version, attempt, fmt.Sprintf("review %d/%d of %s's work", attempt, maxAttempts, editor), turnSpec{
			after: through, through: reviewStart, readOnly: true,
			instruction: fmt.Sprintf("Review %s's latest targeted response and resulting workspace state; do not modify files. Publish prose only if you find a material issue or have genuinely new information. If it is correct and complete, return only the private done:true marker and remain publicly silent.", editor),
		})
		if !o.workflowCurrent(version) {
			return
		}
		if waveFailed(reviews) {
			text := "Review paused after an agent error; use /continue when ready"
			if waveCanceled(reviews) {
				text = "Review paused after an agent cancellation; use /continue when ready"
			}
			terminal = &Event{Type: EventRoundDone, Text: text}
			return
		}
		combined := append([]turnOutcome{editorOutcome}, reviews...)
		if outcomesAgree(combined) {
			o.clearConflict()
			terminal = &Event{Type: EventRoundDone, Wave: attempt, Text: fmt.Sprintf("%s's work passed review on attempt %d", editor, attempt)}
			return
		}
		if attempt == maxAttempts {
			conflict := o.setConflict(attempt, combined)
			terminal = &Event{Type: EventConflict, Participant: conflict.RaisedBy, Wave: attempt, Text: "Review cap reached; your direction is required"}
			return
		}

		correctionStart := o.latestSequence()
		o.send(Event{Type: EventWaveStarted, Participants: []chat.Participant{editor}, Wave: attempt + 1, Text: fmt.Sprintf("%s is applying review feedback", editor)})
		editorOutcome = o.runOne(editor, version, turnSpec{
			after: reviewStart, through: correctionStart,
			instruction: "Reviewers did not yet agree. Reconsider their latest public feedback, correct the work if needed using your configured permissions, and report the revised result.",
		})
		if !o.workflowCurrent(version) {
			return
		}
		if editorOutcome.failed || !editorOutcome.ran {
			text := "Correction paused after an editor error; send a new message or use /continue"
			if editorOutcome.canceled {
				text = "Correction paused after the editor was canceled; send a new message or use /continue"
			}
			terminal = &Event{Type: EventRoundDone, Text: text}
			return
		}
		through = correctionStart
	}
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
	o.send(Event{Type: EventTurnStarted, Participant: participant})

	var draftMu sync.Mutex
	var draft strings.Builder
	emit := o.agentEmitter(ctx, participant, &draftMu, &draft)
	request := o.turnRequest(participant, spec, nil)
	result, err := o.agents[participant].Run(ctx, request, emit)
	outcome.ran = true
	if ctx.Err() != nil || !o.workflowCurrent(version) {
		o.appendInterrupted(participant, &draftMu, &draft)
		o.finishTurn(participant, version, cancel)
		return outcome
	}
	if err != nil {
		o.appendInterrupted(participant, &draftMu, &draft)
		if errors.Is(err, context.Canceled) {
			outcome.failed = true
			outcome.canceled = true
			o.finishTurn(participant, version, cancel)
			return outcome
		}
		o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s: %w", participant, err)})
		outcome.failed = true
		o.finishTurn(participant, version, cancel)
		return outcome
	}

	lastSequence, err := o.recordResult(participant, result, spec.through)
	if err != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: err})
		outcome.failed = true
		o.finishTurn(participant, version, cancel)
		return outcome
	}
	outcome.result = result
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
				o.finishTurn(participant, version, cancel)
				return outcome
			}
			if err != nil {
				o.appendInterrupted(participant, &draftMu, &draft)
				if errors.Is(err, context.Canceled) {
					outcome.failed = true
					outcome.canceled = true
					o.finishTurn(participant, version, cancel)
					return outcome
				}
				o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s retry: %w", participant, err)})
				outcome.failed = true
				o.finishTurn(participant, version, cancel)
				return outcome
			}
			if _, err := o.recordResult(participant, result, retrySpec.through); err != nil {
				o.send(Event{Type: EventError, Participant: participant, Err: err})
				outcome.failed = true
				o.finishTurn(participant, version, cancel)
				return outcome
			}
			outcome.result = result
		}
	}
	o.finishTurn(participant, version, cancel)
	return outcome
}

func (o *Orchestrator) finishTurn(participant chat.Participant, version uint64, cancel context.CancelFunc) {
	cancel()
	o.mu.Lock()
	if current, ok := o.activeTurns[participant]; ok && current.version == version {
		delete(o.activeTurns, participant)
	}
	o.mu.Unlock()
	o.send(Event{Type: EventTurnFinished, Participant: participant})
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
	for _, message := range o.messages {
		if message.Sequence <= spec.through && (message.Sequence > spec.after || message.Sequence > cursor) {
			messages = append(messages, message)
		}
	}
	roomCopy := cloneRoom(o.room)
	configured := o.settings[participant].WithDefaults()
	o.mu.Unlock()
	if spec.readOnly {
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
	if configured.Permissions == chat.PermissionReadOnly {
		prompt = "HOST-ENFORCED TURN PERMISSIONS: READ-ONLY. You cannot edit files or run mutating actions during this turn. Do not claim that you have write or full access.\n\n" + prompt
	}
	return agent.TurnRequest{
		Prompt:       prompt,
		Workspace:    roomCopy.Workspace,
		ReadRoots:    access.EffectiveRoots(roomCopy, participant, chat.AccessRead),
		WriteRoots:   access.EffectiveRoots(roomCopy, participant, chat.AccessReadWrite),
		SystemPrompt: systemPrompt,
		Settings:     configured,
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
	// the sequence of its response. Concurrent peers can post while this turn is
	// running, and those messages must remain eligible for the review wave.
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

func (o *Orchestrator) maxWaves() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.room.MaxWaves < 1 {
		return 3
	}
	return o.room.MaxWaves
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

func waveCanceled(outcomes []turnOutcome) bool {
	for _, outcome := range outcomes {
		if outcome.canceled {
			return true
		}
	}
	return false
}

func outcomesAgree(outcomes []turnOutcome) bool {
	if len(outcomes) == 0 {
		return true
	}
	for _, outcome := range outcomes {
		if !outcome.ran || outcome.failed || !outcome.result.Done || outcome.result.Disagrees {
			return false
		}
	}
	return true
}

func outcomesDisagree(outcomes []turnOutcome) bool {
	for _, outcome := range outcomes {
		if outcome.ran && !outcome.failed && outcome.result.Disagrees {
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
		reasons[raisedBy] = "consensus was not reached"
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

func withoutParticipant(participants []chat.Participant, excluded chat.Participant) []chat.Participant {
	result := make([]chat.Participant, 0, len(participants))
	for _, participant := range participants {
		if participant != excluded {
			result = append(result, participant)
		}
	}
	return result
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
