package room

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

// ControlCoordination is a trusted local control, deliberately not a remote
// command. Resumption rotates identity so delayed reports cannot revive a run.
func (o *Orchestrator) ControlCoordination(action string, now time.Time) (chat.CoordinationView, error) {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	previous := o.room.Coordination.Clone()
	switch action {
	case "start", "resume":
		if previous != nil && (previous.State == "pending" || previous.State == "blocked") {
			return chat.CoordinationView{}, fmt.Errorf("a monitored run is already pending")
		}
		if action == "resume" && (previous == nil || previous.State != "stopped") {
			return chat.CoordinationView{}, fmt.Errorf("no stopped monitored run to resume")
		}
		if action == "start" && previous != nil && previous.State == "stopped" {
			return chat.CoordinationView{}, fmt.Errorf("use /chatgpt monitor resume to resume the stopped run")
		}
		id, err := store.NewID()
		if err != nil {
			return chat.CoordinationView{}, err
		}
		o.room.Coordination = &chat.CoordinationRun{ID: id, State: "pending", StartedAt: now, StartSequence: o.nextSequence}
		if action == "resume" {
			o.room.Coordination = previous.Clone()
			o.room.Coordination.ID = id
			o.room.Coordination.State = "pending"
			o.room.Coordination.Detail = ""
			o.room.Coordination.StartedAt = now
		}
	case "stop":
		if previous == nil {
			return chat.CoordinationView{}, fmt.Errorf("no monitored run")
		}
		o.room.Coordination.State = "stopped"
	default:
		return chat.CoordinationView{}, fmt.Errorf("use monitor start, status, stop, or resume")
	}
	v, _ := o.reconcileCoordinationLocked(now)
	if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
		o.room.Coordination = previous
		return chat.CoordinationView{}, err
	}
	return v, nil
}

// reconcileCoordinationLocked derives progress from durable jobs. It neither
// dispatches work nor regards a successful operation as business completion.
func (o *Orchestrator) reconcileCoordinationLocked(now time.Time) (chat.CoordinationView, bool) {
	r := o.room.Coordination
	if r == nil || r.State == "stopped" {
		return r.View(now, 0, 0), false
	}
	changed := false
	sources := map[uint64]bool{}
	var events []chat.CoordinationEvent
	for _, m := range o.messages {
		if m.Sequence < r.StartSequence || m.Author != chat.ChatGPT {
			continue
		}
		if len(m.RequestedReplies) == 0 && !m.IsWorkflowSource() {
			continue
		}
		sources[m.Sequence] = true
		if m.CreatedAt.After(r.LastAssignmentAt) {
			events = append(events, chat.CoordinationEvent{ID: fmt.Sprintf("assignment:%d", m.Sequence), Kind: "assignment_accepted", At: m.CreatedAt, SourceSequence: m.Sequence})
		}
	}
	pending, waiting := 0, 0
	for _, j := range o.room.Conversations {
		if !sources[j.SourceSequence] {
			continue
		}
		if !j.State.Terminal() {
			pending++
			if j.State != chat.ConversationAnswering {
				waiting++
			}
			continue
		}
		if j.CompletedAt != nil && !j.CompletedAt.Before(r.LastResultAt) {
			events = append(events, chat.CoordinationEvent{ID: "reply:" + j.ID, Kind: "result_available", ResultID: "reply:" + j.ID, At: *j.CompletedAt, SourceSequence: j.SourceSequence, Outcome: string(j.State)})
		}
	}
	for _, w := range o.room.Workflows {
		seq := uint64(0)
		for _, n := range w.SourceSequences {
			if sources[n] {
				seq = n
				break
			}
		}
		if seq == 0 {
			continue
		}
		if w.State == chat.WorkflowQueued || w.State == chat.WorkflowActive || w.State == chat.WorkflowWaiting {
			pending++
			if w.State != chat.WorkflowActive {
				waiting++
			}
			continue
		}
		// needs_attention has no completion time, but is a visible terminal handoff.
		at := w.UpdatedAt
		if w.CompletedAt != nil {
			at = *w.CompletedAt
		}
		if !at.Before(r.LastResultAt) {
			events = append(events, chat.CoordinationEvent{ID: "work:" + w.ID + ":" + string(w.State), Kind: "result_available", ResultID: "work:" + w.ID, At: at, SourceSequence: seq, Outcome: string(w.State)})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	for _, e := range events {
		if !r.Record(e) {
			continue
		}
		changed = true
		if e.Kind == "assignment_accepted" {
			r.LastAssignmentAt = e.At
			r.State = "pending"
			r.Detail = ""
		} else {
			r.LastResultAt = e.At
			if e.Outcome == "answered" || e.Outcome == "completed" {
				r.LastSuccessAt = e.At
			}
		}
	}
	return r.View(now, pending, waiting), changed
}

func (o *Orchestrator) CoordinationStatus(now time.Time) (chat.CoordinationView, error) {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	previous := o.room.Coordination.Clone()
	v, changed := o.reconcileCoordinationLocked(now)
	if changed {
		if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
			o.room.Coordination = previous
			return chat.CoordinationView{}, err
		}
	}
	return v, nil
}

// ReportCoordination accepts only observations for a current run. Notification
// outcomes are panel reports; acknowledgement requires a separate explicit call.
func (o *Orchestrator) ReportCoordination(runID, eventID, kind, resultID, state, detail string, now time.Time) (chat.CoordinationView, error) {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.room.Coordination
	if r == nil || r.ID != runID || r.State == "stopped" {
		return chat.CoordinationView{}, fmt.Errorf("monitored run is absent, stale, or stopped")
	}
	if eventID == "" || len(eventID) > 128 || len(detail) > 1024 {
		return chat.CoordinationView{}, fmt.Errorf("invalid event ID or detail (maximum 1024 bytes)")
	}
	if kind == "coordinator_report" {
		if state != "pending" && state != "blocked" && state != "complete" && state != "stopped" {
			return chat.CoordinationView{}, fmt.Errorf("invalid coordinator state")
		}
		if (state == "pending" || state == "blocked") && strings.TrimSpace(detail) == "" {
			return chat.CoordinationView{}, fmt.Errorf("describe the next action or blocker")
		}
	} else if kind != "notification_attempted" && kind != "host_accepted" && kind != "host_rejected" && kind != "host_unknown" {
		return chat.CoordinationView{}, fmt.Errorf("invalid observation kind")
	}
	previous := r.Clone()
	_, reconciled := o.reconcileCoordinationLocked(now)
	if resultID != "" {
		found := false
		for _, e := range r.Events {
			if e.Kind == "result_available" && e.ResultID == resultID {
				found = true
			}
		}
		if !found {
			o.room.Coordination = previous
			return chat.CoordinationView{}, fmt.Errorf("result is not in this run's retained history")
		}
	}
	if kind != "coordinator_report" && resultID == "" {
		o.room.Coordination = previous
		return chat.CoordinationView{}, fmt.Errorf("notification must identify a retained result")
	}
	if kind != "coordinator_report" && kind != "notification_attempted" {
		found := false
		for _, e := range r.Events {
			if e.ID == eventID && e.Kind == "notification_attempted" && e.ResultID == resultID {
				found = true
			}
		}
		if !found {
			o.room.Coordination = previous
			return chat.CoordinationView{}, fmt.Errorf("notification attempt not recorded")
		}
	}
	for _, e := range r.Events {
		if e.ID == eventID && e.Kind == kind {
			if e.ResultID != resultID || e.Outcome != state || e.Detail != detail {
				o.room.Coordination = previous
				return chat.CoordinationView{}, fmt.Errorf("event ID reused with different content")
			}
			if reconciled {
				if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
					o.room.Coordination = previous
					return chat.CoordinationView{}, err
				}
			}
			v, _ := o.reconcileCoordinationLocked(now)
			return v, nil
		}
	}
	r.Record(chat.CoordinationEvent{ID: eventID, Kind: kind, At: now, ResultID: resultID, Outcome: state, Detail: detail})
	if kind == "coordinator_report" {
		r.State = state
		r.Detail = detail
		if resultID != "" {
			r.AcknowledgedAt = now
			r.AcknowledgedResult = resultID
		}
	}
	if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
		o.room.Coordination = previous
		return chat.CoordinationView{}, err
	}
	v, _ := o.reconcileCoordinationLocked(now)
	return v, nil
}
