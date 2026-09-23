package chat

import "time"

// CoordinationRun records observation, never authorization or a task queue.
// It is independent of the expiring ChatGPT grant and survives room restart.
type CoordinationRun struct {
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
	Detail         string    `json:"detail,omitempty"`
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	At             time.Time `json:"at"`
	ResultID       string    `json:"result_id,omitempty"`
	SourceSequence uint64    `json:"source_sequence,omitempty"`
	Outcome        string    `json:"outcome,omitempty"`
}

type CoordinationView struct {
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
	c.Events = append([]CoordinationEvent(nil), r.Events...)
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
	v := CoordinationView{CoordinationRun: r.Clone(), PendingJobs: pending, WaitingJobs: waiting}
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
	v.ActionNeeded = r.State == "pending" && pending == 0 && v.IdleSeconds >= 600
	v.NoCompletion = r.State == "pending" && v.NoSuccessSeconds >= 3600
	switch {
	case r.State == "stopped":
		v.Summary = "Monitored run stopped; only the local host can resume"
	case r.State == "complete":
		v.Summary = "Coordinator reports run complete (not independently verified)"
	case r.State == "blocked":
		v.Summary = "Coordinator reports blocked: " + r.Detail
	case v.ActionNeeded:
		v.Summary = "Coordinator action needed: no pending jobs for at least 10 minutes"
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
	return v
}
