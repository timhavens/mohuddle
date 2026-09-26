package chat

import (
	"fmt"
	"time"
)

// CoordinationRun retains handoff observations and explicitly registered reviews.
// Its objective is descriptive, not an authorization grant. It survives restart;
// unstarted reviews remain subject to the original expiring ChatGPT grant.
type CoordinationRun struct {
	Objective          *CoordinationObjective `json:"objective,omitempty"`
	Handoffs           []Handoff              `json:"handoffs,omitempty"`
	Continuations      []Continuation         `json:"continuations,omitempty"`
	Panels             []CoordinationPanel    `json:"panels,omitempty"`
	LastNotificationAt time.Time              `json:"last_notification_at,omitzero"`
	NextAction         string                 `json:"next_action,omitempty"`
	Owner              string                 `json:"owner,omitempty"`

	ID                 string              `json:"id"`
	State              string              `json:"state"`
	Detail             string              `json:"detail,omitempty"`
	StartedAt          time.Time           `json:"started_at"`
	StartSequence      uint64              `json:"start_sequence"`
	LastAssignmentAt   time.Time           `json:"last_assignment_at,omitzero"`
	LastResultAt       time.Time           `json:"last_result_at,omitzero"`
	LastSuccessAt      time.Time           `json:"last_success_at,omitzero"`
	AcknowledgedAt     time.Time           `json:"acknowledged_at,omitzero"`
	AcknowledgedResult string              `json:"acknowledged_result,omitempty"`
	Events             []CoordinationEvent `json:"events,omitempty"`
}

type CoordinationEvent struct {
	Update         CoordinatorUpdate `json:"update,omitzero"`
	Detail         string            `json:"detail,omitempty"`
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	At             time.Time         `json:"at"`
	ResultID       string            `json:"result_id,omitempty"`
	SourceSequence uint64            `json:"source_sequence,omitempty"`
	Outcome        string            `json:"outcome,omitempty"`
}

type CoordinationView struct {
	HandoffProtocol int                 `json:"handoff_protocol"`
	Metrics         CoordinationMetrics `json:"metrics"`
	RecoveryStatus  string              `json:"recovery_status"`

	*CoordinationRun
	PendingJobs      int    `json:"pending_jobs"`
	WaitingJobs      int    `json:"waiting_jobs"`
	IdleSeconds      int64  `json:"idle_seconds"`
	NoSuccessSeconds int64  `json:"no_success_seconds"`
	ActionNeeded     bool   `json:"action_needed"`
	NoCompletion     bool   `json:"no_completion"`
	Summary          string `json:"summary"`
}

func (r *CoordinationRun) Clone() *CoordinationRun {
	if r == nil {
		return nil
	}
	c := *r
	if r.Objective != nil {
		obj := *r.Objective
		c.Objective = &obj
	}
	c.Handoffs = append([]Handoff(nil), r.Handoffs...)
	for i := range c.Handoffs {
		c.Handoffs[i].Attempts = append([]NotificationAttempt(nil), r.Handoffs[i].Attempts...)
	}
	c.Continuations = append([]Continuation(nil), r.Continuations...)
	c.Panels = append([]CoordinationPanel(nil), r.Panels...)
	c.Events = append([]CoordinationEvent(nil), r.Events...)
	for i := range c.Events {
		if c.Events[i].Update.Objective != nil {
			objective := *c.Events[i].Update.Objective
			c.Events[i].Update.Objective = &objective
		}
	}
	return &c
}

// Record returns false for an already recorded event. Keep current state and a
// bounded recent history; high-water timestamps survive history eviction.
func (r *CoordinationRun) Record(e CoordinationEvent) bool {
	for _, old := range r.Events {
		if old.ID == e.ID && old.Kind == e.Kind {
			return false
		}
	}
	r.Events = append(r.Events, e)
	if len(r.Events) > 200 {
		r.Events = append([]CoordinationEvent(nil), r.Events[len(r.Events)-200:]...)
	}
	return true
}

func (r *CoordinationRun) View(now time.Time, pending, waiting int) CoordinationView {
	v := CoordinationView{HandoffProtocol: 1, RecoveryStatus: "Delivery availability unknown", CoordinationRun: r.Clone(), PendingJobs: pending, WaitingJobs: waiting}
	if r == nil {
		v.Summary = "Coordination monitoring disabled"
		return v
	}
	if len(v.Events) > 20 {
		v.Events = append([]CoordinationEvent(nil), v.Events[len(v.Events)-20:]...)
	}
	idle := r.StartedAt
	for _, t := range []time.Time{r.LastAssignmentAt, r.LastResultAt} {
		if t.After(idle) {
			idle = t
		}
	}
	success := r.LastSuccessAt
	if success.Before(r.StartedAt) {
		success = r.StartedAt
	}
	v.IdleSeconds = max(0, int64(now.Sub(idle).Seconds()))
	v.NoSuccessSeconds = max(0, int64(now.Sub(success).Seconds()))
	v.ActionNeeded = r.State == "pending" && pending == 0 && v.IdleSeconds >= 180
	v.NoCompletion = r.State == "pending" && v.NoSuccessSeconds >= 3600
	switch {
	case r.State == "stopped":
		v.Summary = "Monitored run stopped; only the local host can resume"
	case r.State == "complete":
		v.Summary = "Coordinator reports run complete (not independently verified)"
	case r.State == "blocked":
		v.Summary = "Coordinator reports blocked: " + r.Detail
	case v.ActionNeeded:
		v.Summary = "Coordinator action needed: no pending jobs for at least 3 minutes"
	case waiting > 0:
		v.Summary = "Assignments waiting for provider or resource capacity"
	case pending > 0:
		v.Summary = "Assignments in progress"
	default:
		v.Summary = "Waiting for coordinator action"
	}
	if v.NoCompletion {
		v.Summary += "; no successful operation completion for at least one hour"
	}

	for _, panel := range r.Panels {
		if now.Sub(panel.SeenAt) <= 45*time.Second {
			v.RecoveryStatus = "Panel reports " + panel.Mode
			if panel.Mode == "enabled" {
				v.RecoveryStatus = "Panel available for follow-ups"
				break
			}
		}
	}
	v.Metrics.Handoffs = len(r.Handoffs)
	var oldest *Handoff
	actionable, retryable := 0, 0
	for i := range v.Handoffs {
		h := &v.Handoffs[i]
		end := now
		if !h.ResolvedAt.IsZero() {
			end = h.ResolvedAt
		}
		age := max(0, int64(end.Sub(h.ReadyAt).Seconds()))
		h.AgeSeconds = age
		v.Metrics.NotificationAttempts += len(h.Attempts)
		for _, a := range h.Attempts {
			if !a.Automatic {
				v.Metrics.ManualInterventions++
			}
		}
		if !h.AcknowledgedAt.IsZero() {
			v.Metrics.Acknowledged++
			v.Metrics.AcknowledgmentSeconds += max(0, int64(h.AcknowledgedAt.Sub(h.ReadyAt).Seconds()))
		}
		if h.AssignmentSequence != 0 {
			v.Metrics.Dispatched++
			v.Metrics.DispatchSeconds += age
		}
		v.Metrics.StalledSeconds += max(0, age-180)
		if h.Open() {
			v.Metrics.Outstanding++
			if h.WaitingFor == "" {
				actionable++
				attempts, rejected := 0, false
				for _, attempt := range h.Attempts {
					if attempt.Automatic {
						attempts++
					}
					rejected = rejected || attempt.Outcome == "host_rejected"
				}
				if attempts < 3 && !rejected {
					retryable++
				}
			}
			h.Stalled = r.State == "pending" && h.WaitingFor == "" && age >= 180
			h.NotificationDue = r.State == "pending" && h.Due(now) && now.Sub(r.LastNotificationAt) >= 20*time.Second
			if h.Stalled {
				v.Metrics.Stalled++
			}
			if h.WaitingFor == "" && (oldest == nil || h.ReadyAt.Before(oldest.ReadyAt)) {
				oldest = h
			}
		}
	}
	if r.State == "pending" && actionable > 0 && retryable == 0 {
		v.RecoveryStatus = "Automatic handoff notifications exhausted or rejected; coordinator action still outstanding"
	}
	if r.State == "pending" && oldest != nil {
		v.ActionNeeded = oldest.Stalled
		v.Summary = fmt.Sprintf("Awaiting %s: result from %s ready %ds ago", oldest.Owner, oldest.Participant, oldest.AgeSeconds)
		if oldest.NextAction != "" {
			v.Summary += "; next: " + oldest.NextAction
		}
		if !oldest.AcknowledgedAt.IsZero() {
			v.Summary += "; acknowledged, next assignment outstanding"
		}
		if oldest.Stalled {
			v.Summary = "Stalled handoff. " + v.Summary
		}
	}
	return v
}
