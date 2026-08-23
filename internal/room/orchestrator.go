package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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

type CorePreferences interface {
	DefaultCorePolicy() chat.CorePolicy
	SetDefaultCorePolicy(chat.CorePolicy) error
}

type EventType string

const (
	EventMessage        EventType = "message"
	EventAgent          EventType = "agent"
	EventRoutingStarted EventType = "routing_started"
	EventWaveStarted    EventType = "wave_started"
	EventTurnStarted    EventType = "turn_started"
	EventTurnFinished   EventType = "turn_finished"
	EventRoundDone      EventType = "round_done"
	EventConflict       EventType = "conflict"
	EventWarning        EventType = "warning"
	EventError          EventType = "error"
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

type CoreStatus struct {
	Policy              chat.CorePolicy
	Inherited           bool
	Active              []chat.Participant
	Promotions          []chat.CorePromotion
	Availability        map[chat.Participant]chat.ParticipantAvailability
	Moderator           chat.Participant
	ModeratorPreference chat.Participant
	ModeratorExplicit   bool
}

type activeTurn struct {
	version uint64
	cancel  context.CancelFunc
}

type turnSpec struct {
	after                  uint64
	through                uint64
	readOnly               bool
	ephemeral              bool
	private                bool
	coreParticipants       []chat.Participant
	publicResponseRequired bool
	instruction            string
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
	corePolicy  chat.CorePolicy

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
	corePolicy := chat.BuiltInCorePolicy()
	if room.CorePolicy != nil {
		corePolicy = room.CorePolicy.WithDefaults()
		canonical := cloneCorePolicy(corePolicy)
		room.CorePolicy = &canonical
	}
	if room.Availability == nil {
		room.Availability = make(map[chat.Participant]chat.ParticipantAvailability)
	}
	ctx, cancel := context.WithCancel(context.Background())
	orchestrator := &Orchestrator{
		store:       roomStore,
		agents:      agentMap,
		room:        room,
		messages:    append([]chat.Message(nil), messages...),
		settings:    make(map[chat.Participant]chat.AgentSettings, len(agentMap)),
		corePolicy:  corePolicy,
		activeTurns: make(map[chat.Participant]activeTurn, len(agentMap)),
		agentGates:  make(map[chat.Participant]*sync.Mutex, len(agentMap)),
		events:      make(chan Event, 512),
		lifetime:    ctx,
		stop:        cancel,
	}
	for participant := range agentMap {
		orchestrator.settings[participant] = chat.AgentSettings{Permissions: participant.DefaultPermissions()}
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
	// A nil room policy inherits personal preferences, which Configure loads
	// immediately in the application. Do not normalize persisted overlays against
	// built-in defaults before those preferences are available.
	if room.CorePolicy != nil || len(room.CorePromotions) == 0 {
		orchestrator.reconcileCoreStateLocked(time.Now())
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
	value.Availability = cloneAvailability(value.Availability)
	value.Grants = append([]chat.AccessGrant(nil), value.Grants...)
	value.CorePromotions = append([]chat.CorePromotion(nil), value.CorePromotions...)
	if value.CorePolicy != nil {
		policy := cloneCorePolicy(*value.CorePolicy)
		value.CorePolicy = &policy
	}
	if value.Conflict != nil {
		conflict := *value.Conflict
		conflict.Reasons = cloneMap(value.Conflict.Reasons)
		value.Conflict = &conflict
	}
	return value
}

func cloneCorePolicy(value chat.CorePolicy) chat.CorePolicy {
	value.Preferred = append([]chat.Participant(nil), value.Preferred...)
	value.Fallbacks = append([]chat.Participant(nil), value.Fallbacks...)
	return value
}

func cloneAvailability(source map[chat.Participant]chat.ParticipantAvailability) map[chat.Participant]chat.ParticipantAvailability {
	if source == nil {
		return nil
	}
	result := make(map[chat.Participant]chat.ParticipantAvailability, len(source))
	for participant, availability := range source {
		if availability.RetryAt != nil {
			retryAt := *availability.RetryAt
			availability.RetryAt = &retryAt
		}
		result[participant] = availability
	}
	return result
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

func (o *Orchestrator) operationalParticipantsLocked(now time.Time) []chat.Participant {
	result := make([]chat.Participant, 0, len(o.agents))
	for _, participant := range o.room.PresentAgents() {
		if o.participantOperationalLocked(participant, now) {
			result = append(result, participant)
		}
	}
	return result
}

func (o *Orchestrator) participantOperationalLocked(participant chat.Participant, now time.Time) bool {
	if !participant.ValidAgent() || !o.room.Present(participant) || o.agents[participant] == nil {
		return false
	}
	availability, unavailable := o.room.Availability[participant]
	if !unavailable {
		return true
	}
	return availability.RetryAt != nil && !now.Before(*availability.RetryAt)
}

func (o *Orchestrator) activeCoreParticipantsLocked(now time.Time) []chat.Participant {
	result := make([]chat.Participant, 0, len(o.corePolicy.Preferred)+len(o.room.CorePromotions))
	seen := make(map[chat.Participant]bool)
	replacements := make(map[chat.Participant]chat.Participant)
	for _, promotion := range o.room.CorePromotions {
		if promotion.Replaces.ValidAgent() && o.participantOperationalLocked(promotion.Participant, now) {
			replacements[promotion.Replaces] = promotion.Participant
		}
	}
	for _, preferred := range o.corePolicy.Preferred {
		participant := preferred
		if replacement := replacements[preferred]; replacement.ValidAgent() {
			participant = replacement
		}
		if participant.ValidAgent() && !seen[participant] {
			seen[participant] = true
			result = append(result, participant)
		}
	}
	for _, promotion := range o.room.CorePromotions {
		if promotion.Replaces.ValidAgent() || !promotion.Participant.ValidAgent() || seen[promotion.Participant] {
			continue
		}
		seen[promotion.Participant] = true
		result = append(result, promotion.Participant)
	}
	return result
}

func (o *Orchestrator) activePresentCoreParticipantsLocked(now time.Time) []chat.Participant {
	active := o.activeCoreParticipantsLocked(now)
	result := make([]chat.Participant, 0, len(active))
	for _, participant := range active {
		if o.participantOperationalLocked(participant, now) {
			result = append(result, participant)
		}
	}
	return result
}

func (o *Orchestrator) hasRoutableCoreLocked(now time.Time) bool {
	if len(o.activePresentCoreParticipantsLocked(now)) > 0 {
		return true
	}
	if o.corePolicy.Failover != chat.CoreFailoverAuto {
		return false
	}
	for _, fallback := range o.corePolicy.Fallbacks {
		if o.participantOperationalLocked(fallback, now) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) CoreStatus() CoreStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	return CoreStatus{
		Policy:              cloneCorePolicy(o.corePolicy),
		Inherited:           o.room.CorePolicy == nil,
		Active:              append([]chat.Participant(nil), o.activePresentCoreParticipantsLocked(time.Now())...),
		Promotions:          append([]chat.CorePromotion(nil), o.room.CorePromotions...),
		Availability:        cloneAvailability(o.room.Availability),
		Moderator:           o.room.Moderator,
		ModeratorPreference: o.room.ModeratorPreference,
		ModeratorExplicit:   o.room.ModeratorExplicit,
	}
}

func (o *Orchestrator) DefaultCorePolicy() chat.CorePolicy {
	o.mu.Lock()
	defer o.mu.Unlock()
	if preferences, ok := o.preferences.(CorePreferences); ok {
		return cloneCorePolicy(preferences.DefaultCorePolicy().WithDefaults())
	}
	return cloneCorePolicy(chat.BuiltInCorePolicy())
}

// RefreshCoreState applies expired cooldowns and deferred automatic
// restoration only while the room is idle, preserving the active workflow's
// core snapshot.
func (o *Orchestrator) RefreshCoreState() error {
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return nil
	}
	now := time.Now()
	changed := o.reconcileCoreStateLocked(now)
	notice := ""
	if changed {
		notice = o.coreStateNoticeLocked(now)
	}
	o.mu.Unlock()
	if !changed {
		return nil
	}
	if err := o.saveRoom(); err != nil {
		return err
	}
	if notice != "" {
		o.send(Event{Type: EventWarning, Text: notice})
	}
	return nil
}

func (o *Orchestrator) reconcileCoreStateLocked(now time.Time) bool {
	changed := false
	o.corePolicy = o.corePolicy.WithDefaults()
	for participant, availability := range o.room.Availability {
		if availability.RetryAt != nil && !now.Before(*availability.RetryAt) {
			delete(o.room.Availability, participant)
			changed = true
		}
	}

	preferred := make(map[chat.Participant]bool, len(o.corePolicy.Preferred))
	for _, participant := range o.corePolicy.Preferred {
		preferred[participant] = true
	}
	used := make(map[chat.Participant]bool)
	filledSlots := make(map[chat.Participant]bool)
	validPromotions := make([]chat.CorePromotion, 0, len(o.room.CorePromotions))
	for _, promotion := range o.room.CorePromotions {
		if !promotion.Participant.ValidAgent() || !promotion.Source.Valid() || preferred[promotion.Participant] || used[promotion.Participant] {
			changed = true
			continue
		}
		if promotion.Replaces != "" && !promotion.Replaces.ValidAgent() {
			changed = true
			continue
		}
		if promotion.Replaces.ValidAgent() && !preferred[promotion.Replaces] {
			changed = true
			continue
		}
		if promotion.Replaces.ValidAgent() && filledSlots[promotion.Replaces] {
			changed = true
			continue
		}
		if !o.participantOperationalLocked(promotion.Participant, now) {
			changed = true
			continue
		}
		if promotion.Source != chat.CorePromotionManual && promotion.Replaces.ValidAgent() &&
			o.participantOperationalLocked(promotion.Replaces, now) && o.corePolicy.Restore == chat.CoreRestoreAuto {
			changed = true
			continue
		}
		used[promotion.Participant] = true
		if promotion.Replaces.ValidAgent() {
			filledSlots[promotion.Replaces] = true
		}
		validPromotions = append(validPromotions, promotion)
	}
	o.room.CorePromotions = validPromotions

	if o.corePolicy.Failover == chat.CoreFailoverAuto {
		replaced := make(map[chat.Participant]bool)
		for _, promotion := range o.room.CorePromotions {
			if promotion.Replaces.ValidAgent() {
				replaced[promotion.Replaces] = true
			}
			used[promotion.Participant] = true
		}
		for _, participant := range o.corePolicy.Preferred {
			if o.participantOperationalLocked(participant, now) || replaced[participant] {
				used[participant] = true
				continue
			}
			for _, fallback := range o.corePolicy.Fallbacks {
				if used[fallback] || !o.participantOperationalLocked(fallback, now) {
					continue
				}
				source := chat.CorePromotionPresence
				reason := fmt.Sprintf("%s is away or unavailable", participant)
				if availability, ok := o.room.Availability[participant]; ok {
					source = chat.CorePromotionAvailability
					reason = availability.Reason
				}
				o.room.CorePromotions = append(o.room.CorePromotions, chat.CorePromotion{
					Participant: fallback, Replaces: participant, Source: source,
					Reason: reason, PromotedAt: now.UTC(),
				})
				used[fallback] = true
				replaced[participant] = true
				changed = true
				break
			}
		}
	}

	active := o.activePresentCoreParticipantsLocked(now)
	if !o.room.ModeratorExplicit && o.room.ModeratorPreference != "" {
		o.room.ModeratorPreference = ""
		changed = true
	}
	if o.room.ModeratorExplicit && !o.room.ModeratorPreference.ValidAgent() {
		o.room.ModeratorExplicit = false
		o.room.ModeratorPreference = ""
		changed = true
	}
	effectiveModerator := chat.Participant("")
	if o.room.ModeratorExplicit && containsParticipant(active, o.room.ModeratorPreference) {
		effectiveModerator = o.room.ModeratorPreference
	} else if o.room.ModeratorExplicit {
		for _, promotion := range o.room.CorePromotions {
			if promotion.Replaces == o.room.ModeratorPreference && containsParticipant(active, promotion.Participant) {
				effectiveModerator = promotion.Participant
				break
			}
		}
	}
	if !effectiveModerator.ValidAgent() && containsParticipant(active, o.room.Moderator) {
		effectiveModerator = o.room.Moderator
	}
	if !effectiveModerator.ValidAgent() && len(active) > 0 {
		effectiveModerator = active[0]
	}
	if o.room.Moderator != effectiveModerator {
		o.room.Moderator = effectiveModerator
		changed = true
	}
	return changed
}

func (o *Orchestrator) Moderator() chat.Participant {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.room.Moderator
}

func (o *Orchestrator) SetModerator(participant chat.Participant) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid moderator %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing the moderator")
	}
	if !containsParticipant(o.activePresentCoreParticipantsLocked(time.Now()), participant) {
		o.mu.Unlock()
		return fmt.Errorf("%s must be an active, present, available core peer to moderate", participant)
	}
	if o.room.Moderator == participant && o.room.ModeratorPreference == participant && o.room.ModeratorExplicit {
		o.mu.Unlock()
		return nil
	}
	o.room.Moderator = participant
	o.room.ModeratorPreference = participant
	o.room.ModeratorExplicit = true
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	_, err := o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("%s is now the moderator", participant))
	return err
}

func (o *Orchestrator) SetModeratorAutomatic() error {
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing the moderator")
	}
	if !o.room.ModeratorExplicit && o.room.ModeratorPreference == "" {
		o.mu.Unlock()
		return nil
	}
	o.room.ModeratorExplicit = false
	o.room.ModeratorPreference = ""
	o.reconcileCoreStateLocked(time.Now())
	moderator := o.room.Moderator
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	_, err := o.appendMessage(chat.System, "", chat.MessageStatus, fmt.Sprintf("moderator selection is automatic; %s currently moderates", moderator))
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
	if present && o.agents[participant] == nil {
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
	previousModerator := o.room.Moderator
	o.reconcileCoreStateLocked(time.Now())
	moderatorChanged := previousModerator != o.room.Moderator
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
	if o.room.CorePolicy != nil {
		o.corePolicy = o.room.CorePolicy.WithDefaults()
		canonical := cloneCorePolicy(o.corePolicy)
		o.room.CorePolicy = &canonical
	} else if corePreferences, ok := preferences.(CorePreferences); ok {
		o.corePolicy = corePreferences.DefaultCorePolicy().WithDefaults()
	} else {
		o.corePolicy = chat.BuiltInCorePolicy()
	}
	for _, participant := range o.participantsLocked() {
		value := chat.AgentSettings{Permissions: participant.DefaultPermissions()}
		if preferences != nil {
			value = preferences.Effective(o.room, participant)
		}
		o.applySettingsLocked(participant, effectiveRoleSettings(participant, mergeSettings(value, o.launch[participant])))
	}
	o.reconcileCoreStateLocked(time.Now())
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) SetCorePolicy(value chat.CorePolicy, personalDefault bool) error {
	if err := value.Validate(); err != nil {
		return err
	}
	value = value.WithDefaults()
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing core peers")
	}
	if personalDefault {
		preferences, ok := o.preferences.(CorePreferences)
		if !ok {
			o.mu.Unlock()
			return fmt.Errorf("personal core settings are unavailable")
		}
		if err := preferences.SetDefaultCorePolicy(value); err != nil {
			o.mu.Unlock()
			return err
		}
		if o.room.CorePolicy != nil {
			o.mu.Unlock()
			return nil
		}
	} else {
		copy := cloneCorePolicy(value)
		o.room.CorePolicy = &copy
	}
	o.corePolicy = cloneCorePolicy(value)
	o.reconcileCoreStateLocked(time.Now())
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) InheritCorePolicy() error {
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing core peers")
	}
	deletePolicy := o.room.CorePolicy != nil
	o.room.CorePolicy = nil
	if preferences, ok := o.preferences.(CorePreferences); ok {
		o.corePolicy = preferences.DefaultCorePolicy().WithDefaults()
	} else {
		o.corePolicy = chat.BuiltInCorePolicy()
	}
	o.reconcileCoreStateLocked(time.Now())
	o.mu.Unlock()
	if !deletePolicy {
		return nil
	}
	return o.saveRoom()
}

func (o *Orchestrator) PromoteCore(participant, replaces chat.Participant) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid core peer %q", participant)
	}
	if replaces != "" && !replaces.ValidAgent() {
		return fmt.Errorf("invalid replaced core peer %q", replaces)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before promoting a core peer")
	}
	if !o.participantOperationalLocked(participant, time.Now()) {
		o.mu.Unlock()
		return fmt.Errorf("%s must be present and available before promotion", participant)
	}
	if replaces.ValidAgent() && !containsParticipant(o.corePolicy.Preferred, replaces) {
		o.mu.Unlock()
		return fmt.Errorf("%s is not a preferred core peer", replaces)
	}
	if containsParticipant(o.corePolicy.Preferred, participant) {
		o.mu.Unlock()
		return fmt.Errorf("%s is already a preferred core peer", participant)
	}
	for index, promotion := range o.room.CorePromotions {
		if promotion.Participant == participant {
			if promotion.Replaces != replaces {
				o.mu.Unlock()
				return fmt.Errorf("%s is already promoted for a different core slot", participant)
			}
			o.room.CorePromotions[index].Source = chat.CorePromotionManual
			o.room.CorePromotions[index].Reason = "manual promotion"
			o.room.CorePromotions[index].PromotedAt = time.Now().UTC()
			o.reconcileCoreStateLocked(time.Now())
			o.mu.Unlock()
			return o.saveRoom()
		}
		if replaces.ValidAgent() && promotion.Replaces == replaces {
			o.mu.Unlock()
			return fmt.Errorf("%s already has a replacement", replaces)
		}
	}
	o.room.CorePromotions = append(o.room.CorePromotions, chat.CorePromotion{
		Participant: participant, Replaces: replaces, Source: chat.CorePromotionManual,
		Reason: "manual promotion", PromotedAt: time.Now().UTC(),
	})
	o.reconcileCoreStateLocked(time.Now())
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) RestoreCore(participant chat.Participant) error {
	if participant != "" && !participant.ValidAgent() {
		return fmt.Errorf("invalid core peer %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before restoring core peers")
	}
	now := time.Now()
	filtered := o.room.CorePromotions[:0]
	matched := false
	for _, promotion := range o.room.CorePromotions {
		selected := participant == "" || promotion.Replaces == participant || promotion.Participant == participant
		if !selected {
			filtered = append(filtered, promotion)
			continue
		}
		matched = true
		if o.corePolicy.Failover == chat.CoreFailoverAuto && promotion.Replaces.ValidAgent() && !o.participantOperationalLocked(promotion.Replaces, now) {
			if participant != "" {
				o.mu.Unlock()
				return fmt.Errorf("%s is still unavailable; mark it available, join it, or set failover to off before restoring", promotion.Replaces)
			}
			// "all" restores every currently restorable slot while leaving an
			// unavailable preferred core's replacement intact.
			filtered = append(filtered, promotion)
			continue
		}
	}
	if participant != "" && !matched {
		o.mu.Unlock()
		return fmt.Errorf("%s has no temporary core promotion", participant)
	}
	o.room.CorePromotions = filtered
	o.reconcileCoreStateLocked(time.Now())
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) SetParticipantAvailability(participant chat.Participant, value *chat.ParticipantAvailability) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	o.mu.Lock()
	if o.activeWork > 0 {
		o.mu.Unlock()
		return fmt.Errorf("stop active work before changing availability")
	}
	if value == nil {
		delete(o.room.Availability, participant)
	} else {
		copy := *value
		copy.Reason = strings.TrimSpace(copy.Reason)
		if copy.Reason == "" {
			copy.Reason = "manually marked unavailable"
		}
		if copy.DetectedAt.IsZero() {
			copy.DetectedAt = time.Now().UTC()
		}
		if copy.Source == "" {
			copy.Source = "manual"
		}
		o.room.Availability[participant] = copy
	}
	o.reconcileCoreStateLocked(time.Now())
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
			value = chat.AgentSettings{Permissions: participant.DefaultPermissions()}
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
		value := chat.AgentSettings{Permissions: participant.DefaultPermissions()}
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
			value = chat.AgentSettings{Permissions: participant.DefaultPermissions()}
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
	value = appsettings.NormalizeFor(participant, value)
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
	value := chat.AgentSettings{Permissions: participant.DefaultPermissions()}
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
	if !value.Permissions.Valid() {
		value.Permissions = participant.DefaultPermissions()
	}
	return value.WithDefaults()
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
	return o.PostWithAttachments(text, nil)
}

func (o *Orchestrator) PostWithAttachments(text string, attachments []chat.Attachment) error {
	target, publicText := parseTarget(text)
	if strings.TrimSpace(publicText) == "" && len(attachments) == 0 {
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
		if !o.participantOperationalLocked(target, time.Now()) {
			o.mu.Unlock()
			return fmt.Errorf("%s is temporarily unavailable; use /core available @%s or wait for its retry time", target, target)
		}
	} else if !o.hasRoutableCoreLocked(time.Now()) {
		o.mu.Unlock()
		return fmt.Errorf("an untagged message needs an active core peer in the room")
	}
	o.room.Conflict = nil
	o.mu.Unlock()

	message, err := o.appendMessageWithAttachments(chat.User, target, chat.MessageText, publicText, attachments)
	if err != nil {
		return err
	}
	if err := o.saveRoom(); err != nil {
		return err
	}

	o.mu.Lock()
	version, moderator, present, cores, notice, err := o.beginWorkflowLocked()
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if notice != "" {
		o.send(Event{Type: EventWarning, Text: notice})
	}
	if target.ValidAgent() {
		o.warnUnsupportedAttachments(attachments, []chat.Participant{target})
		go o.runDirectWorkflow(message.Sequence, target, cores, version)
	} else {
		go o.runModeratedWorkflow(message.Sequence, moderator, present, cores, version, "")
	}
	return nil
}

// Ask runs one explicit concurrent response from each selected present agent.
// Leading @agent tokens select the participants; without them all present
// agents participate.
func (o *Orchestrator) Ask(text string) error {
	return o.AskWithAttachments(text, nil)
}

func (o *Orchestrator) AskWithAttachments(text string, attachments []chat.Attachment) error {
	selected, publicText, err := parseAsk(text)
	if err != nil {
		return err
	}
	if strings.TrimSpace(publicText) == "" && len(attachments) == 0 {
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
		if !o.participantOperationalLocked(participant, time.Now()) {
			o.mu.Unlock()
			return fmt.Errorf("%s is not present and available", participant)
		}
	}
	o.room.Conflict = nil
	o.mu.Unlock()

	message, err := o.appendMessageWithAttachments(chat.User, "", chat.MessageText, publicText, attachments)
	if err != nil {
		return err
	}
	if err := o.saveRoom(); err != nil {
		return err
	}

	o.mu.Lock()
	version, _, _, cores, notice, err := o.beginWorkflowLocked()
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if notice != "" {
		o.send(Event{Type: EventWarning, Text: notice})
	}
	o.warnUnsupportedAttachments(attachments, selected)
	go o.runOneShotWorkflow(message.Sequence, selected, cores, version)
	return nil
}

// Round runs a read-only, sequential discussion among the selected present
// participants and always gives the moderator the final synthesis turn.
// Leading @agent tokens select participants; without them all present agents
// participate.
func (o *Orchestrator) Round(text string) error {
	return o.RoundWithAttachments(text, nil)
}

func (o *Orchestrator) RoundWithAttachments(text string, attachments []chat.Attachment) error {
	selected, publicText, err := parseRound(text)
	if err != nil {
		return err
	}
	if strings.TrimSpace(publicText) == "" && len(attachments) == 0 {
		return fmt.Errorf("message is empty")
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	moderator := o.room.Moderator
	if !o.hasRoutableCoreLocked(time.Now()) {
		o.mu.Unlock()
		return fmt.Errorf("a moderated round needs an active core peer in the room")
	}
	if len(selected) == 0 {
		selected = append([]chat.Participant(nil), o.room.PresentAgents()...)
	}
	for _, participant := range selected {
		if !o.participantOperationalLocked(participant, time.Now()) {
			o.mu.Unlock()
			return fmt.Errorf("%s is not present and available", participant)
		}
	}
	o.room.Conflict = nil
	o.mu.Unlock()

	message, err := o.appendMessageWithAttachments(chat.User, "", chat.MessageText, publicText, attachments)
	if err != nil {
		return err
	}
	if err := o.saveRoom(); err != nil {
		return err
	}

	o.mu.Lock()
	version, moderator, _, cores, notice, err := o.beginWorkflowLocked()
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if notice != "" {
		o.send(Event{Type: EventWarning, Text: notice})
	}
	o.warnUnsupportedAttachments(attachments, selected)
	go o.runRoundWorkflow(message.Sequence, selected, moderator, cores, version)
	return nil
}

func (o *Orchestrator) warnUnsupportedAttachments(attachments []chat.Attachment, participants []chat.Participant) {
	if len(attachments) == 0 {
		return
	}
	for _, participant := range participants {
		if participant == chat.Agy {
			o.send(Event{Type: EventWarning, Participant: participant, Text: "AGY cannot inspect image attachments; the message will continue to the other selected agents."})
			return
		}
	}
}

func (o *Orchestrator) beginWorkflowLocked() (uint64, chat.Participant, []chat.Participant, []chat.Participant, string, error) {
	if o.closed {
		return 0, "", nil, nil, "", fmt.Errorf("room is closed")
	}
	// Invalidate and cancel any prior workflow before changing scheduling roles.
	o.cancelAllLocked()
	o.version++
	now := time.Now()
	changed := o.reconcileCoreStateLocked(now)
	notice := ""
	if changed {
		notice = o.coreStateNoticeLocked(now)
	}
	present := o.operationalParticipantsLocked(now)
	cores := o.activePresentCoreParticipantsLocked(now)
	o.activeWork++
	o.wg.Add(1)
	return o.version, o.room.Moderator, present, cores, notice, nil
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
	if !o.hasRoutableCoreLocked(time.Now()) {
		o.mu.Unlock()
		return fmt.Errorf("continuing an untagged round needs an active core peer in the room")
	}
	after := o.messages[len(o.messages)-1].Sequence
	resumeReason := ""
	if o.room.Conflict != nil {
		resumeReason = strings.TrimSpace(o.room.Conflict.Reason)
	}
	o.room.Conflict = nil
	o.cancelAllLocked()
	o.version++
	now := time.Now()
	changed := o.reconcileCoreStateLocked(now)
	notice := ""
	if changed {
		notice = o.coreStateNoticeLocked(now)
	}
	moderator := o.room.Moderator
	participants := o.operationalParticipantsLocked(now)
	cores := o.activePresentCoreParticipantsLocked(now)
	version := o.version
	o.activeWork++
	o.wg.Add(1)
	o.mu.Unlock()
	if notice != "" {
		o.send(Event{Type: EventWarning, Text: notice})
	}
	if err := o.saveRoom(); err != nil {
		o.finishWorkflow()
		o.wg.Done()
		return err
	}
	go o.runModeratedWorkflow(after, moderator, participants, cores, version, resumeReason)
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

func (o *Orchestrator) runDirectWorkflow(after uint64, participant chat.Participant, cores []chat.Participant, version uint64) {
	defer o.wg.Done()
	defer func() {
		o.finishWorkflow()
	}()
	through := o.latestSequence()
	instruction := "Answer the human directly. This is a one-agent turn: do not request or wait for peer review."
	outcome := o.runOne(participant, version, turnSpec{
		after: after, through: through, coreParticipants: cores, instruction: instruction,
		publicResponseRequired: true,
	})
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

func (o *Orchestrator) runOneShotWorkflow(after uint64, participants, cores []chat.Participant, version uint64) {
	defer o.wg.Done()
	defer o.finishWorkflow()
	through := o.latestSequence()
	outcomes := o.runWave(participants, version, 1, "explicit one-shot", turnSpec{
		after: after, through: through, readOnly: true, coreParticipants: cores,
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

func (o *Orchestrator) runRoundWorkflow(after uint64, selected []chat.Participant, moderator chat.Participant, cores []chat.Participant, version uint64) {
	defer o.wg.Done()
	defer o.finishWorkflow()
	ordered := withoutParticipant(selected, moderator)
	ordered = append(ordered, moderator)
	o.send(Event{Type: EventWaveStarted, Participants: append([]chat.Participant(nil), ordered...), Wave: 1, Text: "read-only moderated round"})

	floorAfter := after
	var failures []chat.Participant
	var concerns []string
	var moderatorOutcome turnOutcome
	for _, participant := range ordered {
		if !o.workflowCurrent(version) {
			return
		}
		through := o.latestSequence()
		instruction := "Contribute your independent view to this read-only moderated discussion. Do not use tools to change the workspace and do not route another participant."
		if participant == moderator {
			instruction = "You are the moderator closing a read-only group round. Synthesize the participants' useful conclusions into one concise public answer for the human. Resolve ordinary differences; mark position disagree only if a material disagreement truly remains. Do not request another participant."
			if len(failures) > 0 {
				instruction += " These requested participants failed or were canceled, so synthesize the available responses without claiming they answered: " + joinParticipants(failures) + "."
			}
			if len(concerns) > 0 {
				instruction += " Private participant metadata reported these material concerns; address them explicitly: " + strings.Join(concerns, "; ") + "."
			}
		}
		outcome := o.runOne(participant, version, turnSpec{after: floorAfter, through: through, readOnly: true, coreParticipants: cores, instruction: instruction})
		floorAfter = through
		if outcome.failed || !outcome.ran {
			failures = appendParticipantOnce(failures, participant)
		} else if participant != moderator {
			concerns = appendOutcomeConcern(concerns, outcome)
		}
		if participant == moderator {
			moderatorOutcome = outcome
		}
	}
	if !o.workflowCurrent(version) {
		return
	}
	if moderatorOutcome.ran && !moderatorOutcome.failed && moderatorOutcome.result.Disagrees {
		conflict := o.setConflict(1, []turnOutcome{moderatorOutcome})
		o.send(Event{Type: EventConflict, Participant: conflict.RaisedBy, Wave: 1, Text: "The moderated group round ended with a material disagreement"})
		return
	}
	o.clearConflict()
	text := "The moderator completed the read-only group round"
	if moderatorOutcome.failed || !moderatorOutcome.ran {
		text = "The read-only group round ended without moderator synthesis"
	}
	if len(failures) > 0 {
		text += "; unavailable participants: " + joinParticipants(failures)
	}
	o.send(Event{Type: EventRoundDone, Wave: 1, Text: text})
}

type leadBid struct {
	Participant   chat.Participant `json:"participant"`
	PreferredLead chat.Participant `json:"preferred_lead"`
	Fit           string           `json:"fit"`
	Reason        string           `json:"reason"`
	Valid         bool             `json:"-"`
}

var sessionLimitResetPattern = regexp.MustCompile(`(?i)(?:you(?:'ve| have) hit (?:your )?session limit|session limit reached)[^\n]*?resets?\s+([0-9]{1,2}:[0-9]{2}\s*(?:am|pm))\s*\(([^)]+)\)`)

func providerAvailability(participant chat.Participant, turnErr error, now time.Time) (chat.ParticipantAvailability, bool) {
	var typed *agent.AvailabilityError
	if errors.As(turnErr, &typed) {
		availability := chat.ParticipantAvailability{
			Reason: strings.TrimSpace(typed.Reason), Source: strings.TrimSpace(typed.Source),
			DetectedAt: now.UTC(), RetryAt: typed.RetryAt, Confidence: strings.TrimSpace(typed.Confidence),
		}
		if availability.Reason == "" {
			availability.Reason = "provider reported a temporary availability limit"
		}
		if availability.Source == "" {
			availability.Source = "provider"
		}
		if availability.Confidence == "" {
			availability.Confidence = "confirmed"
		}
		return availability, true
	}
	message := strings.TrimSpace(turnErr.Error())
	match := sessionLimitResetPattern.FindStringSubmatch(message)
	if len(match) != 3 {
		return chat.ParticipantAvailability{}, false
	}
	availability := chat.ParticipantAvailability{
		Reason: message, Source: "provider-error", DetectedAt: now.UTC(), Confidence: "confirmed",
	}
	retryAt, ok := parseSessionLimitRetry(match[1], match[2], now)
	if !ok {
		// Do not turn an ambiguous timezone or reset time into indefinite
		// automatic downtime. The error remains visible and can be confirmed with
		// /core unavailable once the user knows the intended timestamp.
		return chat.ParticipantAvailability{}, false
	}
	availability.RetryAt = &retryAt
	return availability, true
}

func parseSessionLimitRetry(clock, zone string, now time.Time) (time.Time, bool) {
	location, err := time.LoadLocation(strings.TrimSpace(zone))
	if err != nil {
		return time.Time{}, false
	}
	clock = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(clock), " ", ""))
	parsed, err := time.ParseInLocation("3:04pm", clock, location)
	if err != nil {
		return time.Time{}, false
	}
	localNow := now.In(location)
	retryAt := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	if !retryAt.After(localNow) {
		retryAt = retryAt.AddDate(0, 0, 1)
	}
	return retryAt.UTC(), true
}

func (o *Orchestrator) recordProviderAvailability(participant chat.Participant, turnErr error) {
	availability, ok := providerAvailability(participant, turnErr, time.Now())
	if !ok {
		return
	}
	o.mu.Lock()
	if current, exists := o.room.Availability[participant]; exists && current.Source == "manual" {
		o.mu.Unlock()
		return
	}
	o.room.Availability[participant] = availability
	mode := o.corePolicy.Failover
	affectedCore := chat.Participant("")
	if containsParticipant(o.corePolicy.Preferred, participant) {
		affectedCore = participant
	} else {
		for _, promotion := range o.room.CorePromotions {
			if promotion.Participant == participant && promotion.Replaces.ValidAgent() {
				affectedCore = promotion.Replaces
				break
			}
		}
	}
	o.mu.Unlock()
	detail := fmt.Sprintf("%s is temporarily unavailable", participant)
	if availability.RetryAt != nil {
		detail += "; retry after " + availability.RetryAt.Local().Format(time.RFC3339)
	}
	switch {
	case !affectedCore.ValidAgent():
		detail += "; no preferred core slot is directly affected"
	case mode == chat.CoreFailoverAuto:
		detail += "; automatic core failover will be applied at the next safe routing boundary"
	case mode == chat.CoreFailoverPrompt:
		detail += fmt.Sprintf("; failover requires a choice after this workflow—use /core then /core replace @%s @fallback", affectedCore)
	case mode == chat.CoreFailoverOff:
		detail += "; automatic failover is off—use /core replace manually if needed"
	}
	o.send(Event{Type: EventWarning, Participant: participant, Text: detail})
}

func (o *Orchestrator) coreStateNoticeLocked(now time.Time) string {
	for _, promotion := range o.room.CorePromotions {
		if promotion.Replaces.ValidAgent() && o.participantOperationalLocked(promotion.Replaces, now) && o.corePolicy.Restore == chat.CoreRestorePrompt {
			return fmt.Sprintf("@%s is available again; @%s remains temporary core until you run /core restore @%s", promotion.Replaces, promotion.Participant, promotion.Replaces)
		}
	}
	if o.corePolicy.Failover == chat.CoreFailoverPrompt {
		replaced := make(map[chat.Participant]bool)
		for _, promotion := range o.room.CorePromotions {
			replaced[promotion.Replaces] = true
		}
		for _, preferred := range o.corePolicy.Preferred {
			if !o.participantOperationalLocked(preferred, now) && !replaced[preferred] {
				return fmt.Sprintf("@%s needs a temporary core replacement; use /core replace @%s @fallback", preferred, preferred)
			}
		}
	}
	if len(o.room.CorePromotions) > 0 {
		values := make([]string, 0, len(o.room.CorePromotions))
		for _, promotion := range o.room.CorePromotions {
			if promotion.Replaces.ValidAgent() {
				values = append(values, fmt.Sprintf("@%s for @%s", promotion.Participant, promotion.Replaces))
			} else {
				values = append(values, "@"+string(promotion.Participant))
			}
		}
		return "Active temporary core peers: " + strings.Join(values, ", ")
	}
	return "Preferred core roster restored"
}

const isolatedReadOnlyInstruction = "This is an isolated read-only turn. Use only the supplied room transcript. You have no tools or workspace access. Never request access, suggest that greater access would improve your answer, offer to perform repository work, list missing capabilities, or recommend changing your permissions. Speak only when you have a distinct, relevant contribution."

func (o *Orchestrator) runModeratedWorkflow(after uint64, moderator chat.Participant, present, cores []chat.Participant, version uint64, resumeReason string) {
	defer o.wg.Done()
	defer o.finishWorkflow()
	through := o.latestSequence()
	o.send(Event{Type: EventRoutingStarted, Text: "choosing the core lead"})
	var bids []leadBid
	for attempt := 0; attempt < len(chat.Agents()) && len(cores) > 0; attempt++ {
		bids = o.runLeadBids(through, cores, version)
		if !o.workflowCurrent(version) {
			return
		}
		// A confirmed cooldown discovered during private bidding is known before
		// any public work begins. Reconcile here and rebid only if the active core
		// roster changed, so the limited peer is never dispatched again.
		o.mu.Lock()
		now := time.Now()
		changed := o.reconcileCoreStateLocked(now)
		notice := ""
		if changed {
			notice = o.coreStateNoticeLocked(now)
		}
		moderator = o.room.Moderator
		nextCores := o.activePresentCoreParticipantsLocked(now)
		nextPresent := o.operationalParticipantsLocked(now)
		o.mu.Unlock()
		if notice != "" {
			o.send(Event{Type: EventWarning, Text: notice})
		}
		present = intersectParticipants(present, nextPresent)
		nextCores = intersectParticipants(nextCores, present)
		if sameParticipants(cores, nextCores) {
			break
		}
		cores = nextCores
	}
	lead := selectLead(bids, moderator, cores)
	ordered := coreTurnOrder(lead, moderator, cores)
	if len(ordered) == 0 {
		o.send(Event{Type: EventRoundDone, Text: "Moderated round stopped because no core peer was available"})
		return
	}
	o.send(Event{Type: EventWaveStarted, Participants: append([]chat.Participant(nil), ordered...), Wave: 1, Text: fmt.Sprintf("%s leads; core review follows", lead)})

	invited := make(map[chat.Participant]bool)
	for _, participant := range cores {
		invited[participant] = true
	}
	floorAfter := after
	var moderatorOutcome turnOutcome
	var failures []chat.Participant
	var concerns []string
	for index, participant := range ordered {
		through = o.latestSequence()
		readOnly := index > 0
		instruction := "You are the host-selected lead for this request. Answer the human and perform any authorized work needed. The other core peers will review your response automatically; do not request, address, or wait for another participant."
		if len(ordered) == 1 && participant == moderator {
			instruction = "You are the only present core peer and the room moderator. Answer the human and perform any authorized work needed. After answering, you may invite one remaining optional peer by setting next and done:false; otherwise set done:true. Set position disagree only for a real unresolved material disagreement."
		}
		if resumeReason != "" {
			instruction += " This continues a previously reported material disagreement: " + resumeReason
		}
		if index > 0 {
			instruction = "Review the lead's response and resulting transcript read-only. Publish only a material correction, missing consideration, or useful synthesis; otherwise return only the private done:true marker. Do not route another participant."
			if participant == moderator {
				instruction = moderatorReviewInstruction(present, moderator, invited, resumeReason, failures, concerns)
			}
		}
		outcome := o.runOne(participant, version, turnSpec{after: floorAfter, through: through, readOnly: readOnly, coreParticipants: cores, instruction: instruction})
		if !o.workflowCurrent(version) {
			return
		}
		floorAfter = through
		if outcome.failed || !outcome.ran {
			failures = appendParticipantOnce(failures, participant)
		} else if participant != moderator {
			concerns = appendOutcomeConcern(concerns, outcome)
		}
		if participant == moderator {
			moderatorOutcome = outcome
		}
	}

	// When the moderator was the lead, the other core peers have now reviewed
	// it, so return the floor for a read-only closing decision.
	if ordered[len(ordered)-1] != moderator {
		o.send(Event{Type: EventWaveStarted, Participants: []chat.Participant{moderator}, Wave: 1, Text: "moderator closing review"})
		through = o.latestSequence()
		moderatorOutcome = o.runOne(moderator, version, turnSpec{
			after: floorAfter, through: through, readOnly: true, coreParticipants: cores,
			instruction: moderatorReviewInstruction(present, moderator, invited, resumeReason, failures, concerns),
		})
		if !o.workflowCurrent(version) {
			return
		}
		floorAfter = through
		if moderatorOutcome.failed || !moderatorOutcome.ran {
			failures = appendParticipantOnce(failures, moderator)
		}
	}

	for {
		if moderatorOutcome.failed || !moderatorOutcome.ran {
			o.send(Event{Type: EventRoundDone, Text: "Moderated round ended because the moderator was unavailable"})
			return
		}
		if moderatorOutcome.result.Disagrees {
			conflict := o.setConflict(0, []turnOutcome{moderatorOutcome})
			o.send(Event{Type: EventConflict, Participant: conflict.RaisedBy, Text: "The moderator reported a material disagreement"})
			return
		}
		next := moderatorOutcome.result.Next
		if next == "" && !moderatorOutcome.result.Done {
			next = nextEligibleParticipant(present, moderator, invited)
		}
		if next == "" {
			if !moderatorOutcome.result.Done {
				o.send(Event{Type: EventWarning, Participant: moderator, Text: "The moderator requested another response, but no eligible participant remained; the round ended without a conflict"})
			}
			o.clearConflict()
			o.send(Event{Type: EventRoundDone, Text: fmt.Sprintf("%s ended the moderated round", moderator)})
			return
		}
		if next == moderator || !containsParticipant(present, next) || invited[next] {
			o.send(Event{Type: EventWarning, Participant: moderator, Text: fmt.Sprintf("The moderator selected unavailable or already-heard participant %s; the round ended without a conflict", next)})
			o.clearConflict()
			o.send(Event{Type: EventRoundDone, Text: "Moderated round ended after an invalid floor decision"})
			return
		}
		invited[next] = true
		o.send(Event{Type: EventWaveStarted, Participants: []chat.Participant{next}, Wave: 1, Text: fmt.Sprintf("%s was invited by the moderator", next)})
		through = o.latestSequence()
		instruction := "You were invited by the moderator. Address the current request from the supplied transcript read-only. Your turn returns to the moderator automatically; do not route another participant."
		invitedOutcome := o.runOne(next, version, turnSpec{after: floorAfter, through: through, readOnly: true, coreParticipants: cores, instruction: instruction})
		if !o.workflowCurrent(version) {
			return
		}
		floorAfter = through
		if invitedOutcome.failed || !invitedOutcome.ran {
			failures = appendParticipantOnce(failures, next)
		} else {
			concerns = appendOutcomeConcern(concerns, invitedOutcome)
		}

		o.send(Event{Type: EventWaveStarted, Participants: []chat.Participant{moderator}, Wave: 1, Text: "floor returned to the moderator"})
		through = o.latestSequence()
		moderatorOutcome = o.runOne(moderator, version, turnSpec{
			after: floorAfter, through: through, readOnly: true, coreParticipants: cores,
			instruction: moderatorReviewInstruction(present, moderator, invited, resumeReason, failures, concerns),
		})
		if !o.workflowCurrent(version) {
			return
		}
		floorAfter = through
	}
}

func (o *Orchestrator) runLeadBids(through uint64, participants []chat.Participant, version uint64) []leadBid {
	bids := make([]leadBid, len(participants))
	var wait sync.WaitGroup
	wait.Add(len(participants))
	for index, participant := range participants {
		go func(index int, participant chat.Participant) {
			defer wait.Done()
			instruction := fmt.Sprintf("Private routing bid. Do not perform the task or use tools. Choose the best lead for the current human request from exactly these active core peers: %s. Return only JSON: {\"participant\":%q,\"preferred_lead\":\"one listed participant\",\"fit\":\"high|medium|low\",\"reason\":\"short reason\"}, followed by the required private control marker.", joinParticipants(participants), participant)
			outcome := o.runOne(participant, version, turnSpec{after: 0, through: through, readOnly: true, ephemeral: true, private: true, coreParticipants: participants, instruction: instruction})
			bid := leadBid{Participant: participant, PreferredLead: participant, Fit: "unknown", Reason: "bid unavailable"}
			if outcome.ran && !outcome.failed && json.Unmarshal([]byte(outcome.result.Text), &bid) == nil {
				bid.Participant = participant
				if !containsParticipant(participants, bid.PreferredLead) {
					bid.PreferredLead = participant
				} else {
					bid.Valid = true
				}
				bid.Reason = strings.TrimSpace(bid.Reason)
			}
			bids[index] = bid
		}(index, participant)
	}
	wait.Wait()
	return bids
}

func selectLead(bids []leadBid, moderator chat.Participant, cores []chat.Participant) chat.Participant {
	if len(cores) == 0 {
		return ""
	}
	if len(cores) == 1 {
		return cores[0]
	}
	counts := make(map[chat.Participant]int, len(cores))
	for _, bid := range bids {
		if !bid.Valid || !containsParticipant(cores, bid.PreferredLead) {
			continue
		}
		counts[bid.PreferredLead]++
	}
	var selected chat.Participant
	maxVotes := 0
	tied := false
	for _, participant := range cores {
		votes := counts[participant]
		switch {
		case votes > maxVotes:
			selected, maxVotes, tied = participant, votes, false
		case votes > 0 && votes == maxVotes:
			tied = true
		}
	}
	if maxVotes > 0 && !tied {
		return selected
	}
	if containsParticipant(cores, moderator) {
		return moderator
	}
	return cores[0]
}

func coreTurnOrder(lead, moderator chat.Participant, cores []chat.Participant) []chat.Participant {
	if !containsParticipant(cores, lead) {
		return nil
	}
	result := []chat.Participant{lead}
	for _, participant := range cores {
		if participant == lead || participant == moderator {
			continue
		}
		result = append(result, participant)
	}
	if moderator != lead && containsParticipant(cores, moderator) {
		result = append(result, moderator)
	}
	return result
}

func sameParticipants(left, right []chat.Participant) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func intersectParticipants(values, allowed []chat.Participant) []chat.Participant {
	result := make([]chat.Participant, 0, len(values))
	for _, participant := range values {
		if containsParticipant(allowed, participant) {
			result = append(result, participant)
		}
	}
	return result
}

func moderatorReviewInstruction(present []chat.Participant, moderator chat.Participant, invited map[chat.Participant]bool, resumeReason string, failures []chat.Participant, concerns []string) string {
	available := make([]chat.Participant, 0)
	for _, participant := range present {
		if participant != moderator && !invited[participant] {
			available = append(available, participant)
		}
	}
	instruction := "You are the room moderator performing a read-only closing review; you never moderate the human. Review the core response and any peer feedback. Correct or synthesize only when useful, otherwise remain publicly silent. To invite one remaining optional peer, set next in the private marker and done:false. To end, omit next and set done:true. Set position disagree only for a real unresolved material disagreement; merely waiting for another response is not a conflict."
	if len(available) > 0 {
		instruction += " Remaining optional peers: " + joinParticipants(available) + ". If you set done:false without next, the host will choose the next one in that order."
	} else {
		instruction += " No uninvited optional peer remains."
	}
	if resumeReason != "" {
		instruction += " This round resumed the prior disagreement: " + resumeReason
	}
	if len(failures) > 0 {
		instruction += " These participants failed or were canceled; do not claim they responded: " + joinParticipants(failures) + "."
	}
	if len(concerns) > 0 {
		instruction += " Private peer metadata reported these material concerns; address them before closing: " + strings.Join(concerns, "; ") + "."
	}
	return instruction
}

func nextEligibleParticipant(present []chat.Participant, moderator chat.Participant, invited map[chat.Participant]bool) chat.Participant {
	for _, participant := range present {
		if participant != moderator && !invited[participant] {
			return participant
		}
	}
	return ""
}

func joinParticipants(participants []chat.Participant) string {
	values := make([]string, 0, len(participants))
	for _, participant := range participants {
		values = append(values, string(participant))
	}
	return strings.Join(values, ", ")
}

func appendParticipantOnce(participants []chat.Participant, participant chat.Participant) []chat.Participant {
	if containsParticipant(participants, participant) {
		return participants
	}
	return append(participants, participant)
}

func appendOutcomeConcern(concerns []string, outcome turnOutcome) []string {
	if !outcome.result.Disagrees {
		return concerns
	}
	reason := strings.TrimSpace(outcome.result.ConflictReason)
	if reason == "" {
		reason = "reported a material disagreement"
	}
	return append(concerns, fmt.Sprintf("%s: %s", outcome.participant, reason))
}

func containsParticipant(values []chat.Participant, participant chat.Participant) bool {
	for _, value := range values {
		if value == participant {
			return true
		}
	}
	return false
}

func withoutParticipant(values []chat.Participant, excluded chat.Participant) []chat.Participant {
	result := make([]chat.Participant, 0, len(values))
	for _, participant := range values {
		if participant != excluded {
			result = append(result, participant)
		}
	}
	return result
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
	o.mu.Lock()
	gate := o.agentGates[participant]
	runner := o.agents[participant]
	operational := gate != nil && runner != nil && o.participantOperationalLocked(participant, time.Now())
	o.mu.Unlock()
	if !operational {
		outcome.failed = true
		return outcome
	}
	gate.Lock()
	defer gate.Unlock()
	o.mu.Lock()
	operational = o.participantOperationalLocked(participant, time.Now())
	o.mu.Unlock()
	if !operational {
		outcome.failed = true
		return outcome
	}
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
	result, err := runner.Run(ctx, request, emit)
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
		o.recordProviderAvailability(participant, err)
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
	if request.VoiceOnly && result.AccessRequest != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s isolated read-only turn attempted to request access", participant)})
		outcome.failed = true
		finish()
		return outcome
	}

	lastSequence, err := o.recordResult(participant, result, spec.through, persistentTurn(request))
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
			retryRequest := o.turnRequest(participant, retrySpec, &grant)
			result, err = runner.Run(ctx, retryRequest, emit)
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
				o.recordProviderAvailability(participant, err)
				o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("%s retry: %w", participant, err)})
				outcome.failed = true
				finish()
				return outcome
			}
			if _, err := o.recordResult(participant, result, retrySpec.through, persistentTurn(retryRequest)); err != nil {
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
	configured := effectiveRoleSettings(participant, o.settings[participant])
	if spec.readOnly {
		configured.Permissions = chat.PermissionReadOnly
	}
	voiceOnly := !containsParticipant(spec.coreParticipants, participant) && configured.Permissions == chat.PermissionReadOnly
	cursor := o.room.Sessions[participant].Cursor
	if spec.ephemeral || voiceOnly {
		cursor = 0
	}
	for _, message := range o.messages {
		if message.Sequence <= spec.through && (message.Sequence > spec.after || message.Sequence > cursor) {
			messages = append(messages, message)
		}
	}
	roomCopy := cloneRoom(o.room)
	o.mu.Unlock()
	if temporary != nil {
		roomCopy.Grants = append(roomCopy.Grants, *temporary)
	}
	systemPrompt := agent.RoomProtocolPromptFor(participant, configured)
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
	if voiceOnly {
		systemPrompt += "\n\n" + isolatedReadOnlyInstruction
		prompt = "HOST-ENFORCED TURN MODE: ISOLATED READ-ONLY. You have no tools, filesystem, repository, network, or access-request capability. Do not suggest changing your permissions.\n\n" + prompt
	}
	if configured.Permissions == chat.PermissionReadOnly {
		prompt = "HOST-ENFORCED TURN PERMISSIONS: READ-ONLY. You cannot edit files or run mutating actions during this turn. Do not claim that you have write or full access.\n\n" + prompt
	}
	readRoots := access.EffectiveRoots(roomCopy, participant, chat.AccessRead)
	writeRoots := access.EffectiveRoots(roomCopy, participant, chat.AccessReadWrite)
	attachments := latestAttachments(messages)
	for _, message := range messages {
		for _, attachment := range message.Attachments {
			if directory := filepath.Dir(attachment.Path); directory != "." && directory != "" {
				readRoots = appendUniqueRoot(readRoots, directory)
			}
		}
	}
	if voiceOnly || spec.private {
		readRoots = nil
		writeRoots = nil
	}
	return agent.TurnRequest{
		Prompt:                 prompt,
		Attachments:            attachments,
		Workspace:              roomCopy.Workspace,
		ReadRoots:              readRoots,
		WriteRoots:             writeRoots,
		SystemPrompt:           systemPrompt,
		Settings:               configured,
		Ephemeral:              spec.ephemeral,
		NoTools:                spec.private,
		VoiceOnly:              voiceOnly,
		PublicResponseRequired: spec.publicResponseRequired,
	}
}

func (o *Orchestrator) settingsFor(participant chat.Participant) chat.AgentSettings {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settings[participant].WithDefaults()
}

func persistentTurn(request agent.TurnRequest) bool {
	return !request.VoiceOnly && !request.Ephemeral && !request.NoTools
}

func transcriptPrompt(messages []chat.Message) string {
	var value strings.Builder
	value.WriteString("BEGIN UNTRUSTED ROOM TRANSCRIPT\n")
	for _, message := range messages {
		fmt.Fprintf(&value, "[%d] %s", message.Sequence, message.Author)
		if message.Target.ValidAgent() {
			fmt.Fprintf(&value, " -> %s", message.Target)
		}
		fmt.Fprintf(&value, " (%s):\n%s", message.Kind, message.Text)
		for index, attachment := range message.Attachments {
			fmt.Fprintf(&value, "\n[Image #%d attached: %s]", index+1, attachment.Path)
		}
		value.WriteString("\n\n")
	}
	value.WriteString("END UNTRUSTED ROOM TRANSCRIPT\n\nRespond to the room now.")
	return value.String()
}

func latestAttachments(messages []chat.Message) []chat.Attachment {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Author == chat.User && len(messages[index].Attachments) > 0 {
			return append([]chat.Attachment(nil), messages[index].Attachments...)
		}
	}
	return nil
}

func appendUniqueRoot(roots []string, candidate string) []string {
	for _, root := range roots {
		if root == candidate {
			return roots
		}
	}
	return append(roots, candidate)
}

func (o *Orchestrator) recordResult(participant chat.Participant, result agent.TurnResult, seenThrough uint64, persistSession bool) (uint64, error) {
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
	if persistSession {
		o.mu.Lock()
		session := o.room.Sessions[participant]
		session.ID = result.SessionID
		// Cursor means the newest transcript record supplied to the provider, not
		// the sequence of its response. Other participants can post while this turn
		// is running, and those messages must remain eligible for a later floor turn.
		session.Cursor = seenThrough
		o.room.Sessions[participant] = session
		o.mu.Unlock()
	}
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
	return o.appendMessageWithAttachments(author, target, kind, text, nil)
}

func (o *Orchestrator) appendMessageWithAttachments(author, target chat.Participant, kind chat.MessageKind, text string, attachments []chat.Attachment) (chat.Message, error) {
	o.mu.Lock()
	message := chat.Message{
		Sequence: o.nextSequence, Author: author, Target: target, Kind: kind,
		Text: strings.TrimSpace(text), Attachments: append([]chat.Attachment(nil), attachments...), CreatedAt: time.Now().UTC(),
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
	idle := o.activeWork == 0
	changed := false
	notice := ""
	if idle {
		now := time.Now()
		changed = o.reconcileCoreStateLocked(now)
		if changed {
			notice = o.coreStateNoticeLocked(now)
		}
	}
	o.mu.Unlock()
	// Availability may have been recorded during the workflow without causing an
	// immediate promotion (for example in prompt/off mode or for a non-core
	// participant). Persist the complete room snapshot at the safe boundary.
	if idle {
		if err := o.saveRoom(); err != nil {
			o.send(Event{Type: EventError, Err: fmt.Errorf("save core availability state: %w", err)})
			return
		}
		if changed && notice != "" {
			o.send(Event{Type: EventWarning, Text: notice})
		}
	}
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
		if outcome.failed || !outcome.ran || !outcome.result.Disagrees {
			continue
		}
		reason := strings.TrimSpace(outcome.result.ConflictReason)
		if reason == "" {
			reason = "reported a material disagreement"
		}
		reasons[outcome.participant] = reason
		if raisedBy == "" {
			raisedBy = outcome.participant
		}
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
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		remainder := trimmed[len(prefix):]
		if remainder == "" {
			return participant, ""
		}
		switch remainder[0] {
		case ' ', '\t', '\r', '\n':
			return participant, strings.TrimSpace(remainder)
		case ',', ':':
			return participant, strings.TrimSpace(remainder[1:])
		case '?', '!':
			return participant, strings.TrimSpace(remainder)
		}
	}
	return "", trimmed
}

func parseAsk(value string) ([]chat.Participant, string, error) {
	return parseParticipantSelection(value, "/ask")
}

func parseRound(value string) ([]chat.Participant, string, error) {
	return parseParticipantSelection(value, "/round")
}

func parseParticipantSelection(value, command string) ([]chat.Participant, string, error) {
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
			return nil, "", fmt.Errorf("unknown %s participant %s", command, token)
		}
		if !seen[participant] {
			seen[participant] = true
			participants = append(participants, participant)
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, token))
	}
	return participants, rest, nil
}
