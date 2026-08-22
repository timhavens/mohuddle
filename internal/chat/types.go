package chat

import "time"

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
	switch p {
	case Codex, Claude, Agy, Copilot:
		return true
	default:
		return false
	}
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
	result := make([]Participant, 0, len(agentOrder))
	for _, participant := range agentOrder {
		if r.Present(participant) {
			result = append(result, participant)
		}
	}
	return result
}

type MessageKind string

const (
	MessageText        MessageKind = "message"
	MessageTool        MessageKind = "tool"
	MessageStatus      MessageKind = "status"
	MessageError       MessageKind = "error"
	MessageInterrupted MessageKind = "interrupted"
)

type Message struct {
	ID        string      `json:"id"`
	Sequence  uint64      `json:"sequence"`
	Author    Participant `json:"author"`
	Target    Participant `json:"target,omitempty"`
	Kind      MessageKind `json:"kind"`
	Text      string      `json:"text"`
	CreatedAt time.Time   `json:"created_at"`
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
	MaxWaves  int       `json:"max_waves,omitempty"`
	// MaxTurns and NextOpener are retained so rooms written by older releases
	// continue to load. New orchestration uses MaxWaves.
	MaxTurns   int                           `json:"max_turns,omitempty"`
	NextOpener Participant                   `json:"next_opener,omitempty"`
	Members    map[Participant]bool          `json:"members"`
	Sessions   map[Participant]AgentSession  `json:"sessions"`
	Grants     []AccessGrant                 `json:"grants,omitempty"`
	Settings   map[Participant]AgentSettings `json:"agent_settings,omitempty"`
	Conflict   *ConflictState                `json:"conflict,omitempty"`
}

func NewRoom(id, workspace string, maxWaves int, now time.Time) Room {
	if maxWaves < 1 {
		maxWaves = 3
	}
	room := Room{
		ID:        id,
		Workspace: workspace,
		CreatedAt: now,
		UpdatedAt: now,
		MaxWaves:  maxWaves,
		Members:   map[Participant]bool{Codex: true, Claude: true},
		Sessions:  make(map[Participant]AgentSession, len(agentOrder)),
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
