package room

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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
}

type EventType string

const (
	EventMessage      EventType = "message"
	EventAgent        EventType = "agent"
	EventTurnStarted  EventType = "turn_started"
	EventTurnFinished EventType = "turn_finished"
	EventRoundDone    EventType = "round_done"
	EventConflict     EventType = "conflict"
	EventError        EventType = "error"
)

type Event struct {
	Type        EventType
	Participant chat.Participant
	Message     *chat.Message
	AgentEvent  *agent.Event
	Err         error
	Text        string
}

type Orchestrator struct {
	store       Store
	preferences Preferences
	agents      map[chat.Participant]agent.Agent
	settings    map[chat.Participant]chat.AgentSettings
	launch      map[chat.Participant]chat.AgentSettings

	mu            sync.Mutex
	room          chat.Room
	messages      []chat.Message
	nextSequence  uint64
	currentAgent  chat.Participant
	currentCancel context.CancelFunc
	allVersion    uint64
	agentVersion  map[chat.Participant]uint64
	closed        bool

	turnGate sync.Mutex
	events   chan Event
	lifetime context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
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
	if agentMap[chat.Codex] == nil || agentMap[chat.Claude] == nil {
		return nil, fmt.Errorf("both codex and claude agents are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	orchestrator := &Orchestrator{
		store: roomStore, agents: agentMap, room: room,
		messages:     append([]chat.Message(nil), messages...),
		agentVersion: map[chat.Participant]uint64{chat.Codex: 0, chat.Claude: 0},
		settings: map[chat.Participant]chat.AgentSettings{
			chat.Codex:  {Permissions: chat.PermissionWorkspace},
			chat.Claude: {Permissions: chat.PermissionWorkspace},
		},
		events: make(chan Event, 512), lifetime: ctx, stop: cancel,
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
	value.Sessions = cloneMap(value.Sessions)
	value.Settings = cloneMap(value.Settings)
	value.Grants = append([]chat.AccessGrant(nil), value.Grants...)
	if value.Conflict != nil {
		conflict := *value.Conflict
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

func (o *Orchestrator) Configure(preferences Preferences, launch map[chat.Participant]chat.AgentSettings) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.preferences = preferences
	o.launch = make(map[chat.Participant]chat.AgentSettings, len(launch))
	for participant, value := range launch {
		o.launch[participant] = value
	}
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		value := chat.AgentSettings{Permissions: chat.PermissionWorkspace}
		if preferences != nil {
			value = preferences.Effective(o.room, participant)
		}
		o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
	}
	return o.store.SaveRoom(o.room)
}

func (o *Orchestrator) EffectiveSettings() map[chat.Participant]chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	return map[chat.Participant]chat.AgentSettings{
		chat.Codex: o.settings[chat.Codex], chat.Claude: o.settings[chat.Claude],
	}
}

func (o *Orchestrator) DefaultSettings() map[chat.Participant]chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := map[chat.Participant]chat.AgentSettings{}
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
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
	result := make(map[chat.Participant]chat.AgentSettings, 2)
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
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

func (o *Orchestrator) Models(ctx context.Context, participant chat.Participant) ([]agent.ModelOption, error) {
	if !participant.ValidAgent() {
		return nil, fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	if o.currentAgent.ValidAgent() {
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
	defer o.mu.Unlock()
	if o.currentAgent.ValidAgent() {
		return fmt.Errorf("stop active work before changing settings")
	}
	if value.Permissions == chat.PermissionFull && (o.preferences == nil || !o.preferences.FullAccessAcknowledged()) {
		return fmt.Errorf("full access requires acknowledgement")
	}
	if personalDefault {
		if o.preferences == nil {
			return fmt.Errorf("personal settings are unavailable")
		}
		if err := o.preferences.SetDefault(participant, value); err != nil {
			return err
		}
		if _, overridden := o.room.Settings[participant]; overridden {
			return nil
		}
	} else {
		if o.room.Settings == nil {
			o.room.Settings = make(map[chat.Participant]chat.AgentSettings, 2)
		}
		o.room.Settings[participant] = value
	}
	o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
	return o.store.SaveRoom(o.room)
}

func (o *Orchestrator) InheritAgentSettings(participant chat.Participant) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.currentAgent.ValidAgent() {
		return fmt.Errorf("stop active work before changing settings")
	}
	delete(o.room.Settings, participant)
	value := chat.AgentSettings{Permissions: chat.PermissionWorkspace}
	if o.preferences != nil {
		value = o.preferences.Default(participant)
	}
	o.applySettingsLocked(participant, mergeSettings(value, o.launch[participant]))
	return o.store.SaveRoom(o.room)
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
	target, publicText := parseTarget(text)
	if strings.TrimSpace(publicText) == "" {
		return fmt.Errorf("message is empty")
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	if o.room.Conflict != nil {
		o.room.Conflict = nil
		_ = o.store.SaveRoom(o.room)
	}
	o.mu.Unlock()
	message, err := o.appendMessage(chat.User, target, chat.MessageText, publicText)
	if err != nil {
		return err
	}

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	if target.ValidAgent() {
		o.agentVersion[target]++
		version := o.agentVersion[target]
		allVersion := o.allVersion
		if o.currentAgent == target && o.currentCancel != nil {
			o.currentCancel()
		}
		o.wg.Add(1)
		o.mu.Unlock()
		go o.runTarget(message.Sequence, target, allVersion, version)
		return nil
	}

	o.allVersion++
	allVersion := o.allVersion
	o.agentVersion[chat.Codex]++
	o.agentVersion[chat.Claude]++
	versions := map[chat.Participant]uint64{
		chat.Codex: o.agentVersion[chat.Codex], chat.Claude: o.agentVersion[chat.Claude],
	}
	if o.currentCancel != nil {
		o.currentCancel()
	}
	opener := o.room.NextOpener
	if !opener.ValidAgent() {
		opener = chat.Codex
	}
	o.room.NextOpener = opener.Other()
	_ = o.store.SaveRoom(o.room)
	o.wg.Add(1)
	o.mu.Unlock()
	go o.runDebate(message.Sequence, opener, allVersion, versions)
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
	if o.room.Conflict != nil {
		o.room.Conflict = nil
		_ = o.store.SaveRoom(o.room)
	}
	sequence := o.messages[len(o.messages)-1].Sequence
	o.allVersion++
	version := o.allVersion
	o.agentVersion[chat.Codex]++
	o.agentVersion[chat.Claude]++
	versions := map[chat.Participant]uint64{chat.Codex: o.agentVersion[chat.Codex], chat.Claude: o.agentVersion[chat.Claude]}
	if o.currentCancel != nil {
		o.currentCancel()
	}
	opener := o.room.NextOpener
	if !opener.ValidAgent() {
		opener = chat.Codex
	}
	o.room.NextOpener = opener.Other()
	_ = o.store.SaveRoom(o.room)
	o.wg.Add(1)
	o.mu.Unlock()
	go o.runDebate(sequence, opener, version, versions)
	return nil
}

func (o *Orchestrator) Stop() {
	o.mu.Lock()
	o.allVersion++
	o.agentVersion[chat.Codex]++
	o.agentVersion[chat.Claude]++
	if o.currentCancel != nil {
		o.currentCancel()
	}
	o.mu.Unlock()
}

func (o *Orchestrator) runTarget(after uint64, participant chat.Participant, allVersion, participantVersion uint64) {
	defer o.wg.Done()
	done, ran, conflict := o.runOne(after, participant, allVersion, participantVersion)
	if ran {
		if conflict {
			o.send(Event{Type: EventConflict, Participant: participant, Text: "Agent disagreement requires your direction"})
		} else {
			o.send(Event{Type: EventRoundDone, Text: targetedDoneText(participant, done)})
		}
	}
}

func (o *Orchestrator) runDebate(after uint64, opener chat.Participant, allVersion uint64, versions map[chat.Participant]uint64) {
	defer o.wg.Done()
	participant := opener
	previousDone := false
	completed := 0
	maxTurns := o.maxTurns()
	for completed < maxTurns {
		done, ran, conflict := o.runOne(after, participant, allVersion, versions[participant])
		if !ran {
			return
		}
		if conflict {
			o.send(Event{Type: EventConflict, Participant: participant, Text: "Agent disagreement requires your direction"})
			return
		}
		completed++
		if done && previousDone {
			o.send(Event{Type: EventRoundDone, Text: "Both agents agree the round is complete"})
			return
		}
		previousDone = done
		participant = participant.Other()
		o.mu.Lock()
		if len(o.messages) > 0 {
			after = o.messages[len(o.messages)-1].Sequence
		}
		o.mu.Unlock()
	}
	o.send(Event{Type: EventRoundDone, Text: fmt.Sprintf("Turn limit reached (%d); use /continue for another round", maxTurns)})
}

func (o *Orchestrator) runOne(after uint64, participant chat.Participant, allVersion, participantVersion uint64) (bool, bool, bool) {
	o.turnGate.Lock()
	defer o.turnGate.Unlock()
	if !o.versionCurrent(participant, allVersion, participantVersion) {
		return false, false, false
	}

	ctx, cancel := context.WithCancel(o.lifetime)
	o.mu.Lock()
	o.currentAgent = participant
	o.currentCancel = cancel
	o.mu.Unlock()
	o.send(Event{Type: EventTurnStarted, Participant: participant})
	defer func() {
		cancel()
		o.mu.Lock()
		if o.currentAgent == participant {
			o.currentAgent = ""
			o.currentCancel = nil
		}
		o.mu.Unlock()
		o.send(Event{Type: EventTurnFinished, Participant: participant})
	}()

	request := o.turnRequest(participant, after, nil)
	result, err := o.agents[participant].Run(ctx, request, o.agentEmitter(ctx))
	if err != nil {
		if ctx.Err() == nil {
			o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s: %w", participant, err)})
		}
		return false, false, false
	}
	lastSequence, err := o.recordResult(participant, result)
	if err != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: err})
		return false, false, false
	}
	if result.Disagrees {
		return result.Done, true, true
	}

	settings := o.settingsFor(participant)
	if result.AccessRequest != nil && settings.Permissions != chat.PermissionFull {
		grant, temporary, accepted := o.requestAccess(ctx, participant, *result.AccessRequest)
		if accepted {
			if !temporary {
				o.addGrant(grant)
			}
			retry := o.turnRequest(participant, lastSequence, &grant)
			retry.Prompt = "Access was approved. Continue the work you paused, using the newly granted path.\n\n" + retry.Prompt
			result, err = o.agents[participant].Run(ctx, retry, o.agentEmitter(ctx))
			if err != nil {
				if ctx.Err() == nil {
					o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s retry: %w", participant, err)})
				}
				return false, false, false
			}
			if _, err := o.recordResult(participant, result); err != nil {
				o.send(Event{Type: EventError, Participant: participant, Err: err})
				return false, false, false
			}
		}
	}
	return result.Done, true, result.Disagrees
}

func (o *Orchestrator) agentEmitter(ctx context.Context) func(agent.Event) {
	return func(event agent.Event) {
		if ctx.Err() != nil {
			return
		}
		if event.Type == agent.EventTool && strings.TrimSpace(event.Text) != "" {
			_, _ = o.appendMessage(event.Agent, "", chat.MessageTool, event.Text)
		}
		o.send(Event{Type: EventAgent, AgentEvent: &event})
	}
}

func (o *Orchestrator) turnRequest(participant chat.Participant, after uint64, temporary *chat.AccessGrant) agent.TurnRequest {
	o.mu.Lock()
	messages := make([]chat.Message, 0)
	for _, message := range o.messages {
		if message.Sequence > after || message.Sequence > o.room.Sessions[participant].Cursor {
			messages = append(messages, message)
		}
	}
	roomCopy := o.room
	configured := o.settings[participant].WithDefaults()
	o.mu.Unlock()
	if temporary != nil {
		roomCopy.Grants = append(roomCopy.Grants, *temporary)
	}
	return agent.TurnRequest{
		Prompt:       transcriptPrompt(messages),
		Workspace:    roomCopy.Workspace,
		ReadRoots:    access.EffectiveRoots(roomCopy, participant, chat.AccessRead),
		WriteRoots:   access.EffectiveRoots(roomCopy, participant, chat.AccessReadWrite),
		SystemPrompt: agent.RoomProtocolPromptFor(configured),
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

func (o *Orchestrator) recordResult(participant chat.Participant, result agent.TurnResult) (uint64, error) {
	text := strings.TrimSpace(result.Text)
	if text == "" && result.AccessRequest != nil {
		text = fmt.Sprintf("I need access to %s before I can continue.", result.AccessRequest.Path)
	}
	if text == "" {
		text = "Turn completed without a public response."
	}
	message, err := o.appendMessage(participant, "", chat.MessageText, text)
	if err != nil {
		return 0, err
	}
	o.mu.Lock()
	session := o.room.Sessions[participant]
	session.ID = result.SessionID
	session.Cursor = message.Sequence
	o.room.Sessions[participant] = session
	if result.Disagrees {
		o.room.Conflict = &chat.ConflictState{
			RaisedBy: participant, Reason: strings.TrimSpace(result.ConflictReason), CreatedAt: time.Now().UTC(),
		}
	}
	roomCopy := o.room
	o.mu.Unlock()
	if err := o.store.SaveRoom(roomCopy); err != nil {
		return 0, err
	}
	return message.Sequence, nil
}

func (o *Orchestrator) requestAccess(ctx context.Context, participant chat.Participant, requested agent.AccessRequest) (chat.AccessGrant, bool, bool) {
	path := requested.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(o.room.Workspace, path)
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
			roomCopy := o.room
			o.mu.Unlock()
			_ = o.store.SaveRoom(roomCopy)
			return
		}
	}
	o.room.Grants = append(o.room.Grants, grant)
	roomCopy := o.room
	o.mu.Unlock()
	_ = o.store.SaveRoom(roomCopy)
	_, _ = o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("Granted %s %s access to %s", grant.Participant, grant.Mode, grant.Path))
}

func (o *Orchestrator) RevokeGrant(path string, participant chat.Participant) error {
	canonical, err := access.CanonicalDirectory(path)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if canonical == o.room.Workspace {
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
	return o.store.SaveRoom(o.room)
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

func (o *Orchestrator) versionCurrent(participant chat.Participant, allVersion, participantVersion uint64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.closed && o.allVersion == allVersion && o.agentVersion[participant] == participantVersion
}

func (o *Orchestrator) maxTurns() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.room.MaxTurns < 1 {
		return 4
	}
	return o.room.MaxTurns
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
	o.allVersion++
	if o.currentCancel != nil {
		o.currentCancel()
	}
	o.mu.Unlock()
	o.stop()
	o.wg.Wait()
	participants := []chat.Participant{chat.Codex, chat.Claude}
	sort.Slice(participants, func(i, j int) bool { return participants[i] < participants[j] })
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
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
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

func targetedDoneText(participant chat.Participant, done bool) string {
	if done {
		return fmt.Sprintf("%s completed the targeted turn", participant)
	}
	return fmt.Sprintf("%s completed the targeted turn and expects follow-up", participant)
}
