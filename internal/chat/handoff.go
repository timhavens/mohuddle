package chat

import "time"

// Objective is a coordinator's record of the user's task, not an authority grant.
type CoordinationObjective struct {
	Summary            string `json:"summary"`
	Scope              string `json:"scope"`
	CompletionCriteria string `json:"completion_criteria"`
}

type CoordinatorUpdate struct {
	HandoffOnly bool                   `json:"handoff_only,omitempty"`
	Objective   *CoordinationObjective `json:"objective,omitempty"`
	NextAction  string                 `json:"next_action,omitempty"`
	Owner       string                 `json:"owner,omitempty"`
	WaitingFor  string                 `json:"waiting_for,omitempty"`
	Automatic   bool                   `json:"automatic,omitempty"`
	PanelID     string                 `json:"panel_id,omitempty"`
	Delivery    string                 `json:"delivery,omitempty"`
}

type NotificationAttempt struct {
	PanelID   string    `json:"panel_id,omitempty"`
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Automatic bool      `json:"automatic"`
}

type Handoff struct {
	ID                 string                `json:"id"`
	SourceSequence     uint64                `json:"source_sequence"`
	ResultSequence     uint64                `json:"result_sequence,omitempty"`
	Participant        Participant           `json:"participant,omitempty"`
	Outcome            string                `json:"outcome"`
	ReadyAt            time.Time             `json:"ready_at"`
	AcknowledgedAt     time.Time             `json:"acknowledged_at,omitzero"`
	ResolvedAt         time.Time             `json:"resolved_at,omitzero"`
	Resolution         string                `json:"resolution,omitempty"`
	AssignmentSequence uint64                `json:"assignment_sequence,omitempty"`
	NextAction         string                `json:"next_action,omitempty"`
	Owner              string                `json:"owner"`
	WaitingFor         string                `json:"waiting_for,omitempty"`
	Attempts           []NotificationAttempt `json:"attempts,omitempty"`
	AgeSeconds         int64                 `json:"age_seconds"`
	Stalled            bool                  `json:"stalled"`
	NotificationDue    bool                  `json:"notification_due"`
}

func (h Handoff) Open() bool { return h.Resolution == "" }

// Due is independent of host acceptance and coordinator acknowledgements.
func (h Handoff) Due(now time.Time) bool {
	if !h.Open() || h.WaitingFor != "" {
		return false
	}
	n := 0
	for _, a := range h.Attempts {
		if a.Outcome == "host_rejected" {
			return false
		}
		if a.Automatic {
			n++
		}
	}
	if n >= 3 {
		return false
	}
	delay := []time.Duration{0, time.Minute, 3 * time.Minute}[n]
	return !now.Before(h.ReadyAt.Add(delay))
}

type CoordinationMetrics struct {
	Handoffs              int   `json:"handoffs"`
	Outstanding           int   `json:"outstanding"`
	Stalled               int   `json:"stalled"`
	NotificationAttempts  int   `json:"notification_attempts"`
	Acknowledged          int   `json:"acknowledged"`
	AcknowledgmentSeconds int64 `json:"acknowledgment_seconds"`
	Dispatched            int   `json:"dispatched"`
	DispatchSeconds       int64 `json:"dispatch_seconds"`
	StalledSeconds        int64 `json:"stalled_seconds"`
	ManualInterventions   int   `json:"manual_interventions"`
}

type CoordinationPanel struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`
	SeenAt    time.Time `json:"seen_at"`
	StartedAt time.Time `json:"started_at"`
	Attempts  int       `json:"attempts"`
}

// ContinuationSpec authorizes exactly one read-only assignment. It never
// interprets a review's prose as approval to write or as a new workflow.
type ContinuationSpec struct {
	Target     Participant       `json:"target" jsonschema:"Exact present local reviewer, distinct from the writer."`
	Text       string            `json:"text" jsonschema:"Complete authorized read-only review task. The host attaches the actual parent result; do not include future stages."`
	Effort     string            `json:"effort,omitempty"`
	ReplyClass ConversationClass `json:"reply_class,omitempty"`
}

type CoordinationDispatch struct {
	RunID        string            `json:"run_id"`
	HandoffID    string            `json:"handoff_id,omitempty"`
	Continuation *ContinuationSpec `json:"continuation,omitempty"`
}

func (d *CoordinationDispatch) Clone() *CoordinationDispatch {
	if d == nil {
		return nil
	}
	c := *d
	if d.Continuation != nil {
		spec := *d.Continuation
		c.Continuation = &spec
	}
	return &c
}

type Continuation struct {
	ID                 string           `json:"id"`
	ParentWorkflow     string           `json:"parent_workflow"`
	SourceSequence     uint64           `json:"source_sequence"`
	Spec               ContinuationSpec `json:"spec"`
	State              string           `json:"state"`
	Reason             string           `json:"reason,omitempty"`
	AssignmentSequence uint64           `json:"assignment_sequence,omitempty"`
}

// ChatGPTAssignment is an internal scheduling envelope; API authentication and
// budgets are checked before constructing it.
type ChatGPTAssignment struct {
	Kind         string
	Text         string
	Target       Participant
	Participants []Participant
	ReplyTo      uint64
	Class        ConversationClass
	Effort       EffortSelection
	Route        RouteMetadata
	Coordination *CoordinationDispatch
}
