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
	ConversationFailed         ConversationState = "failed"
	ConversationDismissed      ConversationState = "dismissed"
	ConversationCancelled      ConversationState = "cancelled"
)

func (s ConversationState) Terminal() bool {
	return s == ConversationAnswered || s == ConversationNeedsAttention || s == ConversationFailed || s == ConversationDismissed || s == ConversationCancelled
}

type ConversationActionState string

const ConversationRequiresWork ConversationActionState = "requires_work"

type ConversationInboxCategory string

const (
	ConversationInboxWorking      ConversationInboxCategory = "working"
	ConversationInboxNewAnswer    ConversationInboxCategory = "new_answer"
	ConversationInboxActionNeeded ConversationInboxCategory = "action_needed"
	ConversationInboxHidden       ConversationInboxCategory = "hidden"
)

type ConversationAction string

const (
	ConversationActionCancel  ConversationAction = "cancel"
	ConversationActionDismiss ConversationAction = "dismiss"
	ConversationActionAdd     ConversationAction = "add"
	ConversationActionReplace ConversationAction = "replace"
)

type ConversationInboxCounts struct {
	NewAnswers   int `json:"new"`
	Working      int `json:"working"`
	ActionNeeded int `json:"action_needed"`
}

func CountConversationInbox(jobs []ConversationJob) ConversationInboxCounts {
	var result ConversationInboxCounts
	for _, job := range jobs {
		switch job.DerivedInboxCategory() {
		case ConversationInboxNewAnswer:
			result.NewAnswers++
		case ConversationInboxWorking:
			result.Working++
		case ConversationInboxActionNeeded:
			result.ActionNeeded++
		}
	}
	return result
}

type ConversationClass string

const (
	ConversationQuick    ConversationClass = "quick"
	ConversationStandard ConversationClass = "standard"
	ConversationResearch ConversationClass = "research"
)

func (c ConversationClass) TotalBudget() time.Duration {
	switch c {
	case ConversationResearch:
		return 30 * time.Minute
	default:
		return 10 * time.Minute
	}
}

func (c ConversationClass) AttemptBudget() time.Duration {
	// Attempts receive the remaining class window. Automatic failover is driven
	// by a confirmed provider/process error, never by an arbitrary half-budget
	// timer; the total class deadline remains the only automatic hard stop.
	return c.TotalBudget()
}

type ConversationAttempt struct {
	Participant Participant `json:"participant"`
	Provider    Participant `json:"provider"`
	StartedAt   time.Time   `json:"started_at"`
	Deadline    time.Time   `json:"deadline"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Error       string      `json:"error,omitempty"`
	Window      int         `json:"window,omitempty"`
}

// ConversationJob is the durable lifecycle record for a read-only room
// conversation that may run independently from the single writable workflow.
type ConversationJob struct {
	ID               string                    `json:"id"`
	SourceSequence   uint64                    `json:"source_sequence"`
	State            ConversationState         `json:"state"`
	Class            ConversationClass         `json:"class"`
	WorkflowMode     WorkflowMode              `json:"workflow_mode"`
	Requested        []Participant             `json:"requested,omitempty"`
	Assigned         Participant               `json:"assigned,omitempty"`
	Temporary        bool                      `json:"temporary,omitempty"`
	QueuePosition    int                       `json:"queue_position,omitempty"`
	Attempts         []ConversationAttempt     `json:"attempts,omitempty"`
	AnswerSequence   uint64                    `json:"answer_sequence,omitempty"`
	Unread           bool                      `json:"unread,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
	StartedAt        *time.Time                `json:"started_at,omitempty"`
	Deadline         *time.Time                `json:"deadline,omitempty"`
	RetireAt         *time.Time                `json:"retire_at,omitempty"`
	RetiredAt        *time.Time                `json:"retired_at,omitempty"`
	TerminalReason   string                    `json:"terminal_reason,omitempty"`
	ActionState      ConversationActionState   `json:"action_state,omitempty"`
	FailureSequence  uint64                    `json:"failure_sequence,omitempty"`
	RemoteMessageID  string                    `json:"remote_message_id,omitempty"`
	PromotedSequence uint64                    `json:"promoted_sequence,omitempty"`
	LastActivityAt   time.Time                 `json:"last_activity_at"`
	WaitReason       string                    `json:"wait_reason,omitempty"`
	ExtensionCount   int                       `json:"extension_count,omitempty"`
	AttemptWindow    int                       `json:"attempt_window,omitempty"`
	InboxCategory    ConversationInboxCategory `json:"inbox_category,omitempty"`
	AvailableActions []ConversationAction      `json:"available_actions,omitempty"`
}

// DerivedInboxCategory is the single host-owned visibility rule used by every
// client. Diagnostic fields such as TerminalReason never affect visibility.
func (j ConversationJob) DerivedInboxCategory() ConversationInboxCategory {
	if j.PromotedSequence != 0 {
		return ConversationInboxHidden
	}
	switch j.State {
	case ConversationFinding, ConversationWaiting, ConversationAnswering, ConversationRetrying:
		return ConversationInboxWorking
	case ConversationAnswered:
		if j.Unread {
			return ConversationInboxNewAnswer
		}
	case ConversationNeedsAttention:
		if j.ActionState == ConversationRequiresWork {
			return ConversationInboxActionNeeded
		}
	}
	return ConversationInboxHidden
}

func (j ConversationJob) DerivedAvailableActions() []ConversationAction {
	switch j.DerivedInboxCategory() {
	case ConversationInboxWorking:
		return []ConversationAction{ConversationActionCancel}
	case ConversationInboxNewAnswer:
		return []ConversationAction{ConversationActionDismiss}
	case ConversationInboxActionNeeded:
		return []ConversationAction{ConversationActionAdd, ConversationActionReplace, ConversationActionDismiss}
	default:
		return nil
	}
}

func (j *ConversationJob) DeriveInbox() {
	j.InboxCategory = j.DerivedInboxCategory()
	j.AvailableActions = j.DerivedAvailableActions()
}
