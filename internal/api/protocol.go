package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	Version        = "mohuddle.v1"
	MaxFrameBytes  = 1 << 20
	MaxHistory     = 1000
	MaxRouteHops   = 8
	DefaultHistory = 200
)

type Scope string

const (
	ScopeObserve     Scope = "observe"
	ScopeParticipate Scope = "participate"
	ScopeAdminister  Scope = "administer"
)

func (s Scope) Valid() bool {
	return s == ScopeObserve || s == ScopeParticipate || s == ScopeAdminister
}

type ClientKind string

const (
	ClientLocal  ClientKind = "local"
	ClientPeer   ClientKind = "peer"
	ClientBridge ClientKind = "bridge"
)

func (k ClientKind) Valid() bool {
	return k == ClientLocal || k == ClientPeer || k == ClientBridge
}

type Route struct {
	MessageID        string   `json:"message_id"`
	OriginInstanceID string   `json:"origin_instance_id"`
	OriginClientID   string   `json:"origin_client_id"`
	Hops             []string `json:"hops,omitempty"`
}

type Request struct {
	Version string          `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	RoomID  string          `json:"room_id,omitempty"`
	Route   *Route          `json:"route,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Response struct {
	Version string         `json:"version"`
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Result  any            `json:"result,omitempty"`
	Error   *ProtocolError `json:"error,omitempty"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type HelloRequest struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
}

type HelloResult struct {
	Identity   string     `json:"identity"`
	InstanceID string     `json:"instance_id"`
	Kind       ClientKind `json:"kind"`
	Scopes     []Scope    `json:"scopes"`
}

type JoinRoomRequest struct {
	RoomID string `json:"room_id"`
}

type JoinRoomResult struct {
	RoomID string `json:"room_id"`
}

type HistoryRequest struct {
	After   uint64 `json:"after,omitempty"`
	Through uint64 `json:"through,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type HistoryResult struct {
	Messages       []MessageView `json:"messages"`
	HasMore        bool          `json:"has_more"`
	NextAfter      uint64        `json:"next_after"`
	Through        uint64        `json:"through"`
	LatestSequence uint64        `json:"latest_sequence"`
}

type SendMessageRequest struct {
	Mode string `json:"mode,omitempty"`
	Text string `json:"text"`
}

type InvokeCommandRequest struct {
	Command     string                `json:"command"`
	Participant chat.Participant      `json:"participant,omitempty"`
	Action      chat.RosterActionType `json:"action,omitempty"`
	ExecuteAt   time.Time             `json:"execute_at,omitempty"`
	Reason      string                `json:"reason,omitempty"`
	ActionID    string                `json:"action_id,omitempty"`
}

type RoomView struct {
	ID             string                       `json:"id"`
	CreatedAt      time.Time                    `json:"created_at"`
	UpdatedAt      time.Time                    `json:"updated_at"`
	Moderator      chat.Participant             `json:"moderator,omitempty"`
	Members        map[chat.Participant]bool    `json:"members"`
	CorePolicy     *chat.CorePolicy             `json:"core_policy,omitempty"`
	CorePromotions []chat.CorePromotion         `json:"core_promotions,omitempty"`
	RosterActions  []chat.ScheduledRosterAction `json:"roster_actions,omitempty"`
	PendingInputs  int                          `json:"pending_inputs,omitempty"`
	WorkflowMode   chat.WorkflowMode            `json:"workflow_mode"`
	PendingPlan    *chat.ProposedPlan           `json:"pending_plan,omitempty"`
	Conflict       *chat.ConflictState          `json:"conflict,omitempty"`
}

type AttachmentView struct {
	ID       string              `json:"id"`
	Kind     chat.AttachmentKind `json:"kind"`
	Name     string              `json:"name"`
	MIMEType string              `json:"mime_type"`
	Size     int64               `json:"size"`
	Width    int                 `json:"width,omitempty"`
	Height   int                 `json:"height,omitempty"`
}

type MessageView struct {
	ID               string                 `json:"id"`
	Sequence         uint64                 `json:"sequence"`
	Author           chat.Participant       `json:"author"`
	Target           chat.Participant       `json:"target,omitempty"`
	Kind             chat.MessageKind       `json:"kind"`
	WorkflowMode     chat.WorkflowMode      `json:"workflow_mode,omitempty"`
	Text             string                 `json:"text"`
	Attachments      []AttachmentView       `json:"attachments,omitempty"`
	CorrectionEvents []chat.CorrectionEvent `json:"correction_events,omitempty"`
	Route            *chat.RouteMetadata    `json:"route,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
}

type StatusResult struct {
	Room         RoomView                                          `json:"room"`
	ActiveCores  []chat.Participant                                `json:"active_cores"`
	Availability map[chat.Participant]chat.ParticipantAvailability `json:"availability,omitempty"`
	Corrections  chat.CorrectionCounts                             `json:"corrections"`
	ByAgent      map[chat.Participant]chat.CorrectionCounts        `json:"corrections_by_agent"`
}

type Event struct {
	Version string       `json:"version"`
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	RoomID  string       `json:"room_id"`
	Route   Route        `json:"route"`
	At      time.Time    `json:"at"`
	Payload EventPayload `json:"payload"`
}

type AgentEventView struct {
	Type  string           `json:"type"`
	Agent chat.Participant `json:"agent"`
	Text  string           `json:"text,omitempty"`
}

type EventPayload struct {
	Type         string             `json:"type"`
	Participant  chat.Participant   `json:"participant,omitempty"`
	Participants []chat.Participant `json:"participants,omitempty"`
	Wave         int                `json:"wave,omitempty"`
	Message      *MessageView       `json:"message,omitempty"`
	Agent        *AgentEventView    `json:"agent_event,omitempty"`
	Text         string             `json:"text,omitempty"`
	Role         string             `json:"role,omitempty"`
	Task         string             `json:"task,omitempty"`
	WorkflowMode chat.WorkflowMode  `json:"workflow_mode,omitempty"`
	Queued       int                `json:"queued,omitempty"`
	Error        string             `json:"error,omitempty"`
	StreamGap    uint64             `json:"stream_gap,omitempty"`
	Plan         *chat.ProposedPlan `json:"plan,omitempty"`
}

var identifierPattern = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}\z`)

func validIdentifier(value string) bool { return identifierPattern.MatchString(value) }

func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func decodePayload[T any](request Request) (T, error) {
	var value T
	if len(request.Payload) == 0 {
		request.Payload = json.RawMessage("{}")
	}
	if err := json.Unmarshal(request.Payload, &value); err != nil {
		return value, fmt.Errorf("invalid %s payload: %w", request.Type, err)
	}
	return value, nil
}
