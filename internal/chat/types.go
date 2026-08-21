package chat

import "time"

type Participant string

const (
	User   Participant = "user"
	Codex  Participant = "codex"
	Claude Participant = "claude"
	System Participant = "system"
)

func (p Participant) ValidAgent() bool {
	return p == Codex || p == Claude
}

func (p Participant) Other() Participant {
	if p == Codex {
		return Claude
	}
	return Codex
}

type MessageKind string

const (
	MessageText   MessageKind = "message"
	MessageTool   MessageKind = "tool"
	MessageStatus MessageKind = "status"
	MessageError  MessageKind = "error"
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
	RaisedBy  Participant `json:"raised_by"`
	Reason    string      `json:"reason,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

type Room struct {
	ID         string                        `json:"id"`
	Workspace  string                        `json:"workspace"`
	CreatedAt  time.Time                     `json:"created_at"`
	UpdatedAt  time.Time                     `json:"updated_at"`
	MaxTurns   int                           `json:"max_turns"`
	NextOpener Participant                   `json:"next_opener"`
	Sessions   map[Participant]AgentSession  `json:"sessions"`
	Grants     []AccessGrant                 `json:"grants,omitempty"`
	Settings   map[Participant]AgentSettings `json:"agent_settings,omitempty"`
	Conflict   *ConflictState                `json:"conflict,omitempty"`
}

func NewRoom(id, workspace string, maxTurns int, now time.Time) Room {
	if maxTurns < 1 {
		maxTurns = 4
	}
	return Room{
		ID:         id,
		Workspace:  workspace,
		CreatedAt:  now,
		UpdatedAt:  now,
		MaxTurns:   maxTurns,
		NextOpener: Codex,
		Sessions: map[Participant]AgentSession{
			Codex:  {},
			Claude: {},
		},
		Grants: []AccessGrant{{
			Path:        workspace,
			Mode:        AccessReadWrite,
			Participant: System,
			CreatedAt:   now,
		}},
	}
}
