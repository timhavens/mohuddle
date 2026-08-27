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

// ManualProviderHold is the durable provider-brand hold created by leaving a
// primary participant. It applies to the primary, auxiliaries, and temporary
// responders until the primary explicitly rejoins.
type ManualProviderHold struct {
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type SchedulerState string

const (
	SchedulerQueued         SchedulerState = "queued"
	SchedulerActive         SchedulerState = "active"
	SchedulerWaiting        SchedulerState = "waiting"
	SchedulerQuiet          SchedulerState = "quiet"
	SchedulerNeedsAttention SchedulerState = "needs_attention"
	SchedulerIdle           SchedulerState = "idle"
	SchedulerDone           SchedulerState = "done"
)

func (s SchedulerState) Valid() bool {
	switch s {
	case SchedulerQueued, SchedulerActive, SchedulerWaiting, SchedulerQuiet, SchedulerNeedsAttention, SchedulerIdle, SchedulerDone:
		return true
	default:
		return false
	}
}

type OperationCategory string

const (
	OperationRouting  OperationCategory = "routing"
	OperationReading  OperationCategory = "reading"
	OperationEditing  OperationCategory = "editing"
	OperationTesting  OperationCategory = "testing"
	OperationBuilding OperationCategory = "building"
	OperationWaiting  OperationCategory = "waiting"
	OperationWriting  OperationCategory = "writing"
	OperationOther    OperationCategory = "other"
)

// ParticipantActivity is the sanitized, durable scheduler view. Assignment is
// retained as secondary context; Action is safe to show in compact clients.
type ParticipantActivity struct {
	Participant  Participant       `json:"participant"`
	WorkflowID   string            `json:"workflow_id,omitempty"`
	State        SchedulerState    `json:"state"`
	Action       string            `json:"action,omitempty"`
	Assignment   string            `json:"assignment,omitempty"`
	Role         string            `json:"role,omitempty"`
	Operation    OperationCategory `json:"operation,omitempty"`
	StartedAt    time.Time         `json:"started_at,omitempty"`
	LastUpdateAt time.Time         `json:"last_update_at,omitempty"`
	WaitReason   string            `json:"wait_reason,omitempty"`
	Dependency   string            `json:"dependency,omitempty"`
	Transition   string            `json:"transition,omitempty"`
	Deadline     *time.Time        `json:"deadline,omitempty"`
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
	TurnID           string            `json:"turn_id,omitempty"`
	WorkflowID       string            `json:"workflow_id,omitempty"`
	Author           Participant       `json:"author"`
	Target           Participant       `json:"target,omitempty"`
	Kind             MessageKind       `json:"kind"`
	WorkflowMode     WorkflowMode      `json:"workflow_mode,omitempty"`
	DelegationPolicy DelegationPolicy  `json:"delegation_policy,omitempty"`
	InputIntent      InputIntent       `json:"input_intent,omitempty"`
	IntentConfidence IntentConfidence  `json:"intent_confidence,omitempty"`
	ConversationID   string            `json:"conversation_id,omitempty"`
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

// DelegationPolicy controls how an AI-authored delegation proposal is handled.
// Adaptive is a room preference only; messages are stamped with its resolved
// auto, ask, or manual value so queued work keeps its submission-time meaning.
type DelegationPolicy string

const (
	DelegationAdaptive DelegationPolicy = "adaptive"
	DelegationAuto     DelegationPolicy = "auto"
	DelegationAsk      DelegationPolicy = "ask"
	DelegationManual   DelegationPolicy = "manual"
)

func (p DelegationPolicy) Valid() bool {
	return p == DelegationAdaptive || p == DelegationAuto || p == DelegationAsk || p == DelegationManual
}

func (p DelegationPolicy) WithDefault() DelegationPolicy {
	if !p.Valid() {
		return DelegationAdaptive
	}
	return p
}

// DelegationTask is a bounded, host-validated assignment to one room AI.
type DelegationTask struct {
	Participant Participant `json:"participant"`
	Task        string      `json:"task"`
}

// PendingDelegation is an AI-proposed split awaiting a trusted local choice.
// Reservations are deliberately not persisted or held while this exists.
type PendingDelegation struct {
	ID              string           `json:"id"`
	WorkflowID      string           `json:"workflow_id,omitempty"`
	WorkflowVersion uint64           `json:"workflow_version"`
	SourceSequence  uint64           `json:"source_sequence"`
	Requester       Participant      `json:"requester"`
	Role            string           `json:"role,omitempty"`
	AttemptCharged  bool             `json:"attempt_charged"`
	ProviderLanes   int              `json:"provider_lanes"`
	Tasks           []DelegationTask `json:"tasks"`
	Joins           []Participant    `json:"joins,omitempty"`
	Leaves          []Participant    `json:"leaves,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

func (p PendingDelegation) Valid() bool {
	return strings.TrimSpace(p.ID) != "" && p.WorkflowVersion > 0 && p.SourceSequence > 0 && p.Requester.ValidAgent() && len(p.Tasks) > 0
}

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

// StreamMode controls how provisional provider response text is presented.
// Stable keeps it out of the transcript, Live shows a separate current-turn
// panel, and History also retains bounded sanitized turn details in the room.
type StreamMode string

const (
	StreamStable  StreamMode = "stable"
	StreamLive    StreamMode = "live"
	StreamHistory StreamMode = "history"
)

func (m StreamMode) Valid() bool {
	return m == StreamStable || m == StreamLive || m == StreamHistory
}

func (m StreamMode) WithDefault() StreamMode {
	if !m.Valid() {
		return StreamStable
	}
	return m
}

type TurnRecordState string

const (
	TurnRecordFinal       TurnRecordState = "final"
	TurnRecordSilent      TurnRecordState = "silent"
	TurnRecordInterrupted TurnRecordState = "interrupted"
)

// TurnRecord contains only response text and tool summaries that the host
// already exposed publicly while a turn ran. Provider reasoning blocks and
// private control markers are never stored here. Interrupted takes precedence
// when a later continuation fails; FinalSequence may still identify a message
// the same turn published before that interruption.
type TurnRecord struct {
	ID            string          `json:"id"`
	WorkflowID    string          `json:"workflow_id,omitempty"`
	Participant   Participant     `json:"participant"`
	Role          string          `json:"role,omitempty"`
	Task          string          `json:"task,omitempty"`
	State         TurnRecordState `json:"state"`
	Drafts        []string        `json:"drafts,omitempty"`
	Tools         []string        `json:"tools,omitempty"`
	FinalSequence uint64          `json:"final_sequence,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   time.Time       `json:"completed_at"`
}

func (s AgentSettings) WithDefaults() AgentSettings {
	if !s.Permissions.Valid() {
		s.Permissions = PermissionWorkspace
	}
	return s
}

type ConflictState struct {
	WorkflowID string                 `json:"workflow_id,omitempty"`
	RaisedBy   Participant            `json:"raised_by"`
	Reason     string                 `json:"reason,omitempty"`
	Wave       int                    `json:"wave,omitempty"`
	Reasons    map[Participant]string `json:"reasons,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
}

// WorkflowState is the durable lifecycle of one independently schedulable
// human request. Provider turns may come and go while the workflow remains
// active or waiting on a named dependency.
type WorkflowState string

const (
	WorkflowQueued         WorkflowState = "queued"
	WorkflowActive         WorkflowState = "active"
	WorkflowWaiting        WorkflowState = "waiting"
	WorkflowNeedsAttention WorkflowState = "needs_attention"
	WorkflowCompleted      WorkflowState = "completed"
	WorkflowCancelled      WorkflowState = "cancelled"
	WorkflowInterrupted    WorkflowState = "interrupted"
)

func (s WorkflowState) Valid() bool {
	switch s {
	case WorkflowQueued, WorkflowActive, WorkflowWaiting, WorkflowNeedsAttention, WorkflowCompleted, WorkflowCancelled, WorkflowInterrupted:
		return true
	default:
		return false
	}
}

func (s WorkflowState) Terminal() bool {
	return s == WorkflowCompleted || s == WorkflowCancelled || s == WorkflowInterrupted
}

// WorkflowResource describes the host resource whose ownership controls safe
// overlap. Workspace writers serialize until isolated worktrees are available.
type WorkflowResource string

const (
	WorkflowReadOnly       WorkflowResource = "read_only"
	WorkflowExternal       WorkflowResource = "external"
	WorkflowWorkspaceWrite WorkflowResource = "workspace_write"
)

func (r WorkflowResource) Valid() bool {
	return r == WorkflowReadOnly || r == WorkflowExternal || r == WorkflowWorkspaceWrite
}

// WorkflowRecord is the persisted scheduler record for one request. Runtime
// cancellation functions remain private to the orchestrator; everything a
// restart or UI needs to explain the workflow is durable here.
type WorkflowRecord struct {
	ID                string             `json:"id"`
	Generation        uint64             `json:"generation"`
	SourceSequences   []uint64           `json:"source_sequences"`
	Target            Participant        `json:"target,omitempty"`
	Lead              Participant        `json:"lead,omitempty"`
	Mode              WorkflowMode       `json:"mode"`
	DelegationPolicy  DelegationPolicy   `json:"delegation_policy"`
	Resource          WorkflowResource   `json:"resource"`
	State             WorkflowState      `json:"state"`
	WaitReason        string             `json:"wait_reason,omitempty"`
	Dependency        string             `json:"dependency,omitempty"`
	PendingPlan       *ProposedPlan      `json:"pending_plan,omitempty"`
	PendingDelegation *PendingDelegation `json:"pending_delegation,omitempty"`
	Conflict          *ConflictState     `json:"conflict,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	CompletedAt       *time.Time         `json:"completed_at,omitempty"`
}

func (w WorkflowRecord) Valid() bool {
	return strings.TrimSpace(w.ID) != "" && w.Generation > 0 && len(w.SourceSequences) > 0 &&
		w.Mode.WithDefault().Valid() && w.DelegationPolicy.Valid() && w.Resource.Valid() && w.State.Valid() && !w.CreatedAt.IsZero()
}

// InputResolution resolves one ambiguous transcript message without reposting
// its text as a second user message.
type InputResolution struct {
	SourceSequence uint64      `json:"source_sequence"`
	WorkflowID     string      `json:"workflow_id,omitempty"`
	Intent         InputIntent `json:"intent"`
	ResolvedAt     time.Time   `json:"resolved_at"`
}

// CurrentRoomSchemaVersion identifies the newest durable room representation
// this binary can safely read. Older rooms are migrated during load; newer
// rooms must be rejected rather than partially decoded and overwritten.
const CurrentRoomSchemaVersion = 1

type Room struct {
	SchemaVersion int       `json:"schema_version,omitempty"`
	ID            string    `json:"id"`
	Workspace     string    `json:"workspace"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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
	ManualProviderHolds map[Participant]ManualProviderHold      `json:"manual_provider_holds,omitempty"`
	Activities          map[Participant]ParticipantActivity     `json:"activities,omitempty"`
	RosterActions       []ScheduledRosterAction                 `json:"roster_actions,omitempty"`
	Members             map[Participant]bool                    `json:"members"`
	Sessions            map[Participant]AgentSession            `json:"sessions"`
	Grants              []AccessGrant                           `json:"grants,omitempty"`
	Settings            map[Participant]AgentSettings           `json:"agent_settings,omitempty"`
	WorkflowMode        WorkflowMode                            `json:"workflow_mode,omitempty"`
	DelegationPolicy    DelegationPolicy                        `json:"delegation_policy,omitempty"`
	StreamMode          StreamMode                              `json:"stream_mode,omitempty"`
	TurnHistory         []TurnRecord                            `json:"turn_history,omitempty"`
	Workflows           map[string]WorkflowRecord               `json:"workflows,omitempty"`
	InputResolutions    map[uint64]InputResolution              `json:"input_resolutions,omitempty"`
	PendingPlan         *ProposedPlan                           `json:"pending_plan,omitempty"`
	PendingDelegation   *PendingDelegation                      `json:"pending_delegation,omitempty"`
	Conflict            *ConflictState                          `json:"conflict,omitempty"`
	PendingInputs       []uint64                                `json:"pending_inputs,omitempty"`
	PendingRoutes       []uint64                                `json:"pending_routes,omitempty"`
	Conversations       []ConversationJob                       `json:"conversations,omitempty"`
}

func NewRoom(id, workspace string, maxWaves int, now time.Time) Room {
	if maxWaves < 1 {
		maxWaves = 3
	}
	room := Room{
		SchemaVersion:    CurrentRoomSchemaVersion,
		ID:               id,
		Workspace:        workspace,
		CreatedAt:        now,
		UpdatedAt:        now,
		MaxWaves:         maxWaves,
		Moderator:        Codex,
		WorkflowMode:     WorkflowExecute,
		DelegationPolicy: DelegationAdaptive,
		StreamMode:       StreamStable,
		Members:          map[Participant]bool{Codex: true, Claude: true},
		Sessions:         make(map[Participant]AgentSession, len(agentOrder)),
		Workflows:        make(map[string]WorkflowRecord),
		InputResolutions: make(map[uint64]InputResolution),
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
