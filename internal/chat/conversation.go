package chat

import "time"

// InputIntent is the host-owned routing decision for a human room message.
// It is persisted so identical input cannot change meaning at a later workflow
// boundary or after restart.
type InputIntent string

const (
	InputConversation InputIntent = "conversation"
	InputWork         InputIntent = "work"
	InputAmbiguous    InputIntent = "ambiguous"
)

func (i InputIntent) Valid() bool {
	return i == InputConversation || i == InputWork || i == InputAmbiguous
}

type IntentConfidence string

const (
	IntentHigh IntentConfidence = "high"
	IntentLow  IntentConfidence = "low"
)

type ConversationState string

const (
	ConversationFinding        ConversationState = "finding"
	ConversationWaiting        ConversationState = "waiting"
	ConversationAnswering      ConversationState = "answering"
	ConversationRetrying       ConversationState = "retrying"
	ConversationAnswered       ConversationState = "answered"
	ConversationNeedsAttention ConversationState = "needs_attention"
	ConversationCancelled      ConversationState = "cancelled"
)

func (s ConversationState) Terminal() bool {
	return s == ConversationAnswered || s == ConversationNeedsAttention || s == ConversationCancelled
}

type ConversationClass string

const (
	ConversationQuick    ConversationClass = "quick"
	ConversationStandard ConversationClass = "standard"
	ConversationResearch ConversationClass = "research"
)

func (c ConversationClass) TotalBudget() time.Duration {
	switch c {
	case ConversationQuick:
		return 20 * time.Second
	case ConversationResearch:
		return 120 * time.Second
	default:
		return 60 * time.Second
	}
}

func (c ConversationClass) AttemptBudget() time.Duration {
	return c.TotalBudget() / 2
}

type ConversationAttempt struct {
	Participant Participant `json:"participant"`
	Provider    Participant `json:"provider"`
	StartedAt   time.Time   `json:"started_at"`
	Deadline    time.Time   `json:"deadline"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// ConversationJob is the durable lifecycle record for a read-only room
// conversation that may run independently from the single writable workflow.
type ConversationJob struct {
	ID               string                `json:"id"`
	SourceSequence   uint64                `json:"source_sequence"`
	State            ConversationState     `json:"state"`
	Class            ConversationClass     `json:"class"`
	WorkflowMode     WorkflowMode          `json:"workflow_mode"`
	Requested        []Participant         `json:"requested,omitempty"`
	Assigned         Participant           `json:"assigned,omitempty"`
	Temporary        bool                  `json:"temporary,omitempty"`
	QueuePosition    int                   `json:"queue_position,omitempty"`
	Attempts         []ConversationAttempt `json:"attempts,omitempty"`
	AnswerSequence   uint64                `json:"answer_sequence,omitempty"`
	Unread           bool                  `json:"unread,omitempty"`
	CreatedAt        time.Time             `json:"created_at"`
	UpdatedAt        time.Time             `json:"updated_at"`
	StartedAt        *time.Time            `json:"started_at,omitempty"`
	Deadline         *time.Time            `json:"deadline,omitempty"`
	RetireAt         *time.Time            `json:"retire_at,omitempty"`
	RetiredAt        *time.Time            `json:"retired_at,omitempty"`
	TerminalReason   string                `json:"terminal_reason,omitempty"`
	RemoteMessageID  string                `json:"remote_message_id,omitempty"`
	PromotedSequence uint64                `json:"promoted_sequence,omitempty"`
	LastActivityAt   time.Time             `json:"last_activity_at"`
}
