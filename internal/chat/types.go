package chat

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Participant string

const (
	User    Participant = "user"
	Codex   Participant = "codex"
	Claude  Participant = "claude"
	Agy     Participant = "agy"
	Copilot Participant = "copilot"
	System  Participant = "system"
)

var agentOrder = [...]Participant{Codex, Claude, Agy, Copilot}

func Agents() []Participant {
	return append([]Participant(nil), agentOrder[:]...)
}

func DefaultAgents() []Participant {
	return []Participant{Codex, Claude}
}

func (p Participant) ValidAgent() bool {
	return p.Provider() != ""
}

func (p Participant) Provider() Participant {
	switch p {
	case Codex, Claude, Agy, Copilot:
		return p
	}
	value := string(p)
	for _, provider := range agentOrder {
		prefix := string(provider) + "-"
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(value, prefix)
		index, err := strconv.Atoi(suffix)
		if err == nil && index > 0 && strconv.Itoa(index) == suffix {
			return provider
		}
	}
	return ""
}

func (p Participant) IsPrimaryAgent() bool {
	return p.ValidAgent() && p.Provider() == p
}

func (p Participant) IsAuxiliary() bool {
	return p.ValidAgent() && !p.IsPrimaryAgent()
}

func (p Participant) AuxiliaryIndex() int {
	if !p.IsAuxiliary() {
		return 0
	}
	index, _ := strconv.Atoi(strings.TrimPrefix(string(p), string(p.Provider())+"-"))
	return index
}

func AuxiliaryParticipant(provider Participant, index int) (Participant, bool) {
	if !provider.IsPrimaryAgent() || index < 1 {
		return "", false
	}
	return Participant(fmt.Sprintf("%s-%d", provider, index)), true
}

func OrderedParticipants(values []Participant) []Participant {
	result := append([]Participant(nil), values...)
	providerOrder := make(map[Participant]int, len(agentOrder))
	for index, provider := range agentOrder {
		providerOrder[provider] = index
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftProvider, rightProvider := result[left].Provider(), result[right].Provider()
		if providerOrder[leftProvider] != providerOrder[rightProvider] {
			return providerOrder[leftProvider] < providerOrder[rightProvider]
		}
		if result[left].AuxiliaryIndex() != result[right].AuxiliaryIndex() {
			return result[left].AuxiliaryIndex() < result[right].AuxiliaryIndex()
		}
		return result[left] < result[right]
	})
	return result
}

func (p Participant) DefaultPermissions() PermissionProfile {
	if p.IsAuxiliary() || p.Provider() == Agy || p.Provider() == Copilot {
		return PermissionReadOnly
	}
	return PermissionWorkspace
}

type CoreFailoverMode string

const (
	CoreFailoverOff    CoreFailoverMode = "off"
	CoreFailoverPrompt CoreFailoverMode = "prompt"
	CoreFailoverAuto   CoreFailoverMode = "auto"
)

func (m CoreFailoverMode) Valid() bool {
	return m == CoreFailoverOff || m == CoreFailoverPrompt || m == CoreFailoverAuto
}

type CoreRestoreMode string

const (
	CoreRestoreManual CoreRestoreMode = "manual"
	CoreRestorePrompt CoreRestoreMode = "prompt"
	CoreRestoreAuto   CoreRestoreMode = "auto"
)

func (m CoreRestoreMode) Valid() bool {
	return m == CoreRestoreManual || m == CoreRestorePrompt || m == CoreRestoreAuto
}

type CorePolicy struct {
	Preferred []Participant    `json:"preferred"`
	Fallbacks []Participant    `json:"fallbacks,omitempty"`
	Failover  CoreFailoverMode `json:"failover,omitempty"`
	Restore   CoreRestoreMode  `json:"restore,omitempty"`
}

func BuiltInCorePolicy() CorePolicy {
	return CorePolicy{
		Preferred: []Participant{Codex, Claude},
		Fallbacks: []Participant{Agy, Copilot},
		Failover:  CoreFailoverAuto,
		Restore:   CoreRestoreAuto,
	}
}

func (p CorePolicy) WithDefaults() CorePolicy {
	builtIn := BuiltInCorePolicy()
	if len(p.Preferred) == 0 {
		p.Preferred = append([]Participant(nil), builtIn.Preferred...)
		if p.Fallbacks == nil {
			p.Fallbacks = append([]Participant(nil), builtIn.Fallbacks...)
		}
	}
	p.Preferred = uniqueValidParticipants(p.Preferred, nil)
	if len(p.Preferred) == 0 {
		p.Preferred = append([]Participant(nil), builtIn.Preferred...)
		if p.Fallbacks == nil {
			p.Fallbacks = append([]Participant(nil), builtIn.Fallbacks...)
		}
	}
	preferred := make(map[Participant]bool, len(p.Preferred))
	for _, participant := range p.Preferred {
		preferred[participant] = true
	}
	p.Fallbacks = uniqueValidParticipants(p.Fallbacks, preferred)
	if !p.Failover.Valid() {
		p.Failover = builtIn.Failover
	}
	if !p.Restore.Valid() {
		p.Restore = builtIn.Restore
	}
	return p
}

func (p CorePolicy) Validate() error {
	if len(p.Preferred) == 0 {
		return fmt.Errorf("at least one preferred core peer is required")
	}
	if !p.Failover.Valid() {
		return fmt.Errorf("invalid core failover mode %q", p.Failover)
	}
	if !p.Restore.Valid() {
		return fmt.Errorf("invalid core restoration mode %q", p.Restore)
	}
	seen := make(map[Participant]string, len(p.Preferred)+len(p.Fallbacks))
	for _, participant := range p.Preferred {
		if !participant.IsPrimaryAgent() {
			return fmt.Errorf("invalid preferred core peer %q", participant)
		}
		if prior := seen[participant]; prior != "" {
			return fmt.Errorf("%s appears more than once in core policy", participant)
		}
		seen[participant] = "preferred"
	}
	for _, participant := range p.Fallbacks {
		if !participant.IsPrimaryAgent() {
			return fmt.Errorf("invalid fallback core peer %q", participant)
		}
		if prior := seen[participant]; prior != "" {
			return fmt.Errorf("%s appears in both %s and fallback core peers", participant, prior)
		}
		seen[participant] = "fallback"
	}
	return nil
}

func uniqueValidParticipants(values []Participant, excluded map[Participant]bool) []Participant {
	result := make([]Participant, 0, len(values))
	seen := make(map[Participant]bool, len(values))
	for _, participant := range values {
		if !participant.IsPrimaryAgent() || seen[participant] || excluded[participant] {
			continue
		}
		seen[participant] = true
		result = append(result, participant)
	}
	return result
}

type CorePromotionSource string

const (
	CorePromotionManual       CorePromotionSource = "manual"
	CorePromotionPresence     CorePromotionSource = "presence"
	CorePromotionAvailability CorePromotionSource = "availability"
)

func (s CorePromotionSource) Valid() bool {
	return s == CorePromotionManual || s == CorePromotionPresence || s == CorePromotionAvailability
}

type CorePromotion struct {
	Participant Participant         `json:"participant"`
	Replaces    Participant         `json:"replaces,omitempty"`
	Source      CorePromotionSource `json:"source"`
	Reason      string              `json:"reason,omitempty"`
	PromotedAt  time.Time           `json:"promoted_at"`
}

type ParticipantAvailability struct {
	Reason     string     `json:"reason"`
	Source     string     `json:"source,omitempty"`
	DetectedAt time.Time  `json:"detected_at"`
	RetryAt    *time.Time `json:"retry_at,omitempty"`
	Confidence string     `json:"confidence,omitempty"`
}

type RosterActionType string

const (
	RosterActionJoin  RosterActionType = "join"
	RosterActionLeave RosterActionType = "leave"
)

func (a RosterActionType) Valid() bool {
	return a == RosterActionJoin || a == RosterActionLeave
}

type RosterActionStatus string

const (
	RosterActionPending   RosterActionStatus = "pending"
	RosterActionExecuted  RosterActionStatus = "executed"
	RosterActionCancelled RosterActionStatus = "cancelled"
	RosterActionFailed    RosterActionStatus = "failed"
)

func (s RosterActionStatus) Valid() bool {
	return s == RosterActionPending || s == RosterActionExecuted || s == RosterActionCancelled || s == RosterActionFailed
}

// ScheduledRosterAction is the durable host audit record for a future roster
// mutation. Only an explicit human command may create one. Completed and
// cancelled records are retained so a transcript suggestion can never masquerade
// as authorization after a restart.
type ScheduledRosterAction struct {
	ID           string             `json:"id"`
	Action       RosterActionType   `json:"action"`
	Participant  Participant        `json:"participant"`
	ExecuteAt    time.Time          `json:"execute_at"`
	CreatedAt    time.Time          `json:"created_at"`
	AuthorizedBy Participant        `json:"authorized_by"`
	Reason       string             `json:"reason,omitempty"`
	Status       RosterActionStatus `json:"status"`
	CompletedAt  *time.Time         `json:"completed_at,omitempty"`
	Detail       string             `json:"detail,omitempty"`
}

type CorrectionEventType string

const (
	CorrectionOffered   CorrectionEventType = "offered"
	CorrectionDisputed  CorrectionEventType = "disputed"
	CorrectionAccepted  CorrectionEventType = "accepted"
	CorrectionRetracted CorrectionEventType = "retracted"
)

func (t CorrectionEventType) Valid() bool {
	return t == CorrectionOffered || t == CorrectionDisputed || t == CorrectionAccepted || t == CorrectionRetracted
}

// CorrectionEvent is immutable metadata stored atomically with the public
// message that declared it. Offered events carry full attribution; lifecycle
// events reference the offered correction's message sequence.
type CorrectionEvent struct {
	Type               CorrectionEventType `json:"type"`
	CorrectionSequence uint64              `json:"correction_sequence"`
	CorrectedSequence  uint64              `json:"corrected_sequence,omitempty"`
	Proposer           Participant         `json:"proposer,omitempty"`
	Target             Participant         `json:"target,omitempty"`
}

type CorrectionStatus string

const (
	CorrectionPendingStatus   CorrectionStatus = "pending"
	CorrectionDisputedStatus  CorrectionStatus = "disputed"
	CorrectionAcceptedStatus  CorrectionStatus = "accepted"
	CorrectionRetractedStatus CorrectionStatus = "retracted"
)

type Correction struct {
	CorrectionSequence uint64
	CorrectedSequence  uint64
	Proposer           Participant
	Target             Participant
	Status             CorrectionStatus
	StatusSequence     uint64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type CorrectionCounts struct {
	Offered          int `json:"offered"`
	Accepted         int `json:"accepted"`
	Retracted        int `json:"retracted"`
	Pending          int `json:"pending"`
	AcceptedReceived int `json:"accepted_received"`
}

// CorrectionLedger validates and replays immutable transcript events in message
// order. Invalid, duplicate, unauthorized, out-of-order, and post-terminal
// events are ignored, so corrupted transcript metadata cannot inflate counts.
func CorrectionLedger(messages []Message) []Correction {
	ordered := append([]Message(nil), messages...)
	sequenceCounts := make(map[uint64]int, len(ordered))
	for _, message := range ordered {
		if message.Sequence != 0 {
			sequenceCounts[message.Sequence]++
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	bySequence := make(map[uint64]Message, len(ordered))
	for _, message := range ordered {
		if message.Sequence != 0 && sequenceCounts[message.Sequence] == 1 {
			bySequence[message.Sequence] = message
		}
	}
	ledger := make([]Correction, 0)
	corrections := make(map[uint64]int)
	for _, message := range ordered {
		if message.Sequence == 0 || sequenceCounts[message.Sequence] != 1 || message.Kind != MessageText || !message.Author.ValidAgent() || strings.TrimSpace(message.Text) == "" {
			continue
		}
		var offered, resolution *CorrectionEvent
		offeredCount, resolutionCount := 0, 0
		for index := range message.CorrectionEvents {
			event := &message.CorrectionEvents[index]
			if !event.Type.Valid() {
				continue
			}
			if event.Type == CorrectionOffered {
				offeredCount++
				offered = event
				continue
			}
			resolutionCount++
			resolution = event
		}
		// Live ingestion permits at most one offer and one lifecycle action in a
		// response. Apply the same shape during replay so corrupted metadata
		// cannot create events the host would never have recorded.
		if resolutionCount == 1 {
			event := *resolution
			index, ok := corrections[event.CorrectionSequence]
			if !ok || event.CorrectionSequence >= message.Sequence {
				// Invalid lifecycle references do not prevent an otherwise valid
				// offer in the same response from being replayed.
			} else {
				correction := &ledger[index]
				if correction.Status != CorrectionAcceptedStatus && correction.Status != CorrectionRetractedStatus {
					valid := false
					switch event.Type {
					case CorrectionAccepted:
						if message.Author == correction.Target {
							correction.Status = CorrectionAcceptedStatus
							valid = true
						}
					case CorrectionDisputed:
						if message.Author == correction.Target && correction.Status != CorrectionDisputedStatus {
							correction.Status = CorrectionDisputedStatus
							valid = true
						}
					case CorrectionRetracted:
						if message.Author == correction.Proposer {
							correction.Status = CorrectionRetractedStatus
							valid = true
						}
					}
					if valid {
						correction.StatusSequence = message.Sequence
						correction.UpdatedAt = message.CreatedAt
					}
				}
			}
		}
		if offeredCount == 1 {
			event := *offered
			corrected, ok := bySequence[event.CorrectedSequence]
			if !ok || event.CorrectionSequence != message.Sequence || event.CorrectedSequence >= message.Sequence || corrected.Kind != MessageText || !corrected.Author.ValidAgent() || strings.TrimSpace(corrected.Text) == "" || corrected.Author == message.Author || event.Proposer != message.Author || event.Target != corrected.Author {
				continue
			}
			if _, duplicate := corrections[event.CorrectionSequence]; duplicate {
				continue
			}
			corrections[event.CorrectionSequence] = len(ledger)
			ledger = append(ledger, Correction{
				CorrectionSequence: event.CorrectionSequence,
				CorrectedSequence:  event.CorrectedSequence,
				Proposer:           event.Proposer,
				Target:             event.Target,
				Status:             CorrectionPendingStatus,
				CreatedAt:          message.CreatedAt,
				UpdatedAt:          message.CreatedAt,
			})
		}
	}
	return ledger
}

// CorrectionStatistics derives room and participant totals from the validated
// transcript ledger so counters cannot drift from their auditable source events.
func CorrectionStatistics(messages []Message) (CorrectionCounts, map[Participant]CorrectionCounts) {
	byAgent := make(map[Participant]CorrectionCounts, len(agentOrder))
	for _, participant := range agentOrder {
		byAgent[participant] = CorrectionCounts{}
	}
	var room CorrectionCounts
	for _, correction := range CorrectionLedger(messages) {
		room.Offered++
		proposer := byAgent[correction.Proposer]
		proposer.Offered++
		switch correction.Status {
		case CorrectionAcceptedStatus:
			room.Accepted++
			room.AcceptedReceived++
			proposer.Accepted++
			target := byAgent[correction.Target]
			target.AcceptedReceived++
			byAgent[correction.Target] = target
		case CorrectionRetractedStatus:
			room.Retracted++
			proposer.Retracted++
		case CorrectionPendingStatus, CorrectionDisputedStatus:
			room.Pending++
			proposer.Pending++
		}
		byAgent[correction.Proposer] = proposer
	}
	return room, byAgent
}

func ParseParticipant(value string) (Participant, bool) {
	p := Participant(value)
	return p, p.ValidAgent()
}

func (r Room) Present(participant Participant) bool {
	if !participant.ValidAgent() {
		return false
	}
	if r.Members == nil {
		return participant == Codex || participant == Claude
	}
	return r.Members[participant]
}

func (r Room) PresentAgents() []Participant {
	if r.Members == nil {
		return DefaultAgents()
	}
	result := make([]Participant, 0, len(r.Members))
	for participant, present := range r.Members {
		if present && participant.ValidAgent() {
			result = append(result, participant)
		}
	}
	return OrderedParticipants(result)
}

type MessageKind string

const (
	MessageText        MessageKind = "message"
	MessageTool        MessageKind = "tool"
	MessageStatus      MessageKind = "status"
	MessageError       MessageKind = "error"
	MessageInterrupted MessageKind = "interrupted"
)

type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
)

// Attachment is room-owned media associated with a human message. Path is an
// absolute host path so provider transports can consume the file directly; the
// files themselves live in the private room state directory.
type Attachment struct {
	ID       string         `json:"id"`
	Kind     AttachmentKind `json:"kind"`
	Name     string         `json:"name"`
	MIMEType string         `json:"mime_type"`
	Path     string         `json:"path"`
	Size     int64          `json:"size"`
	Width    int            `json:"width,omitempty"`
	Height   int            `json:"height,omitempty"`
}

type ComposerHistoryEntry struct {
	Text        string       `json:"text,omitempty"`
	Pastes      []string     `json:"pastes,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

type Message struct {
	ID               string            `json:"id"`
	Sequence         uint64            `json:"sequence"`
	Author           Participant       `json:"author"`
	Target           Participant       `json:"target,omitempty"`
	Kind             MessageKind       `json:"kind"`
	WorkflowMode     WorkflowMode      `json:"workflow_mode,omitempty"`
	Text             string            `json:"text"`
	Attachments      []Attachment      `json:"attachments,omitempty"`
	CorrectionEvents []CorrectionEvent `json:"correction_events,omitempty"`
	AcceptedPlan     *ProposedPlan     `json:"accepted_plan,omitempty"`
	Route            *RouteMetadata    `json:"route,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

// WorkflowMode captures whether a human request may execute authorized work
// or must remain a host-enforced, read-only planning workflow. It is distinct
// from each agent's saved permission profile and is stamped onto human
// messages so queued work cannot change meaning when the room mode changes.
type WorkflowMode string

const (
	WorkflowExecute WorkflowMode = "execute"
	WorkflowPlan    WorkflowMode = "plan"
)

func (m WorkflowMode) Valid() bool {
	return m == WorkflowExecute || m == WorkflowPlan
}

func (m WorkflowMode) WithDefault() WorkflowMode {
	if !m.Valid() {
		return WorkflowExecute
	}
	return m
}

func (m WorkflowMode) PlanOnly() bool { return m.WithDefault() == WorkflowPlan }

// RouteMetadata preserves the authenticated origin of a message that entered
// through the local API or a future federation transport. Hops are instance
// identities already traversed by the message.
type RouteMetadata struct {
	MessageID        string   `json:"message_id"`
	OriginInstanceID string   `json:"origin_instance_id"`
	OriginClientID   string   `json:"origin_client_id"`
	Hops             []string `json:"hops,omitempty"`
}

type AccessMode string

const (
	AccessRead      AccessMode = "read"
	AccessReadWrite AccessMode = "read_write"
)

type AccessGrant struct {
	Path        string      `json:"path"`
	Mode        AccessMode  `json:"mode"`
	Participant Participant `json:"participant"`
	CreatedAt   time.Time   `json:"created_at"`
}

type AgentSession struct {
	ID     string `json:"id,omitempty"`
	Cursor uint64 `json:"cursor,omitempty"`
}

type PermissionProfile string

const (
	PermissionReadOnly  PermissionProfile = "read-only"
	PermissionWorkspace PermissionProfile = "workspace"
	PermissionFull      PermissionProfile = "full"
)

func (p PermissionProfile) Valid() bool {
	return p == PermissionReadOnly || p == PermissionWorkspace || p == PermissionFull
}

type AgentSettings struct {
	Model       string            `json:"model,omitempty"`
	Effort      string            `json:"effort,omitempty"`
	Permissions PermissionProfile `json:"permissions,omitempty"`
}

type ProgressMode string

const (
	ProgressCompact  ProgressMode = "compact"
	ProgressDetailed ProgressMode = "detailed"
	ProgressOff      ProgressMode = "off"
)

func (m ProgressMode) Valid() bool {
	return m == ProgressCompact || m == ProgressDetailed || m == ProgressOff
}

func (m ProgressMode) WithDefault() ProgressMode {
	if !m.Valid() {
		return ProgressCompact
	}
	return m
}

func (s AgentSettings) WithDefaults() AgentSettings {
	if !s.Permissions.Valid() {
		s.Permissions = PermissionWorkspace
	}
	return s
}

type ConflictState struct {
	RaisedBy  Participant            `json:"raised_by"`
	Reason    string                 `json:"reason,omitempty"`
	Wave      int                    `json:"wave,omitempty"`
	Reasons   map[Participant]string `json:"reasons,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
}

type Room struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// MaxWaves, MaxTurns, and NextOpener are retained so rooms written by older
	// releases continue to load. Moderated orchestration is structurally bounded.
	MaxWaves            int                                     `json:"max_waves,omitempty"`
	MaxTurns            int                                     `json:"max_turns,omitempty"`
	NextOpener          Participant                             `json:"next_opener,omitempty"`
	Moderator           Participant                             `json:"moderator,omitempty"`
	ModeratorPreference Participant                             `json:"moderator_preference,omitempty"`
	ModeratorExplicit   bool                                    `json:"moderator_explicit,omitempty"`
	CorePolicy          *CorePolicy                             `json:"core_policy,omitempty"`
	CorePromotions      []CorePromotion                         `json:"core_promotions,omitempty"`
	Availability        map[Participant]ParticipantAvailability `json:"availability,omitempty"`
	RosterActions       []ScheduledRosterAction                 `json:"roster_actions,omitempty"`
	Members             map[Participant]bool                    `json:"members"`
	Sessions            map[Participant]AgentSession            `json:"sessions"`
	Grants              []AccessGrant                           `json:"grants,omitempty"`
	Settings            map[Participant]AgentSettings           `json:"agent_settings,omitempty"`
	WorkflowMode        WorkflowMode                            `json:"workflow_mode,omitempty"`
	PendingPlan         *ProposedPlan                           `json:"pending_plan,omitempty"`
	Conflict            *ConflictState                          `json:"conflict,omitempty"`
	PendingInputs       []uint64                                `json:"pending_inputs,omitempty"`
}

func NewRoom(id, workspace string, maxWaves int, now time.Time) Room {
	if maxWaves < 1 {
		maxWaves = 3
	}
	room := Room{
		ID:           id,
		Workspace:    workspace,
		CreatedAt:    now,
		UpdatedAt:    now,
		MaxWaves:     maxWaves,
		Moderator:    Codex,
		WorkflowMode: WorkflowExecute,
		Members:      map[Participant]bool{Codex: true, Claude: true},
		Sessions:     make(map[Participant]AgentSession, len(agentOrder)),
		Grants: []AccessGrant{{
			Path:        workspace,
			Mode:        AccessReadWrite,
			Participant: System,
			CreatedAt:   now,
		}},
	}
	for _, participant := range agentOrder {
		room.Sessions[participant] = AgentSession{}
	}
	return room
}
