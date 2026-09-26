package room

import (
	"fmt"
	"reflect"
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
			for i := range o.room.Coordination.Handoffs {
				h := &o.room.Coordination.Handoffs[i]
				if h.Resolution == "coordinator_reported_stopped" {
					h.Resolution, h.ResolvedAt = "", time.Time{}
				}
			}
		}
	case "stop":
		if previous == nil {
			return chat.CoordinationView{}, fmt.Errorf("no monitored run")
		}
		o.room.Coordination.State = "stopped"
		o.cancelContinuationsLocked("Host stopped the run")
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
	sourceMessages := map[uint64]chat.Message{}
	var events []chat.CoordinationEvent
	for _, m := range o.messages {
		if m.Sequence < r.StartSequence || (m.Author != chat.ChatGPT && m.Author != chat.User) {
			continue
		}
		if len(m.RequestedReplies) == 0 && (m.WorkflowID == "" || !m.IsWorkflowSource()) {
			continue
		}
		sources[m.Sequence] = true
		sourceMessages[m.Sequence] = m
		if m.Coordination != nil && m.Coordination.RunID == r.ID && !o.chatgptPending[m.WorkflowID] {
			accepted := false
			if w, ok := o.room.Workflows[m.WorkflowID]; ok && w.WaitReason != "could not persist ChatGPT work request" {
				accepted = true
			}
			for _, j := range o.room.Conversations {
				if j.SourceSequence == m.Sequence && j.TerminalReason != "could not persist peer reply request" && !o.chatgptPending[j.ID] {
					accepted = true
				}
			}
			if accepted {
				if h := handoff(r, m.Coordination.HandoffID); h != nil && (h.Open() || h.Resolution == "coordinator_reported_blocked") {
					h.Resolution, h.ResolvedAt, h.AssignmentSequence = "assignment_accepted", m.CreatedAt, m.Sequence
					changed = true
				}
				if spec := m.Coordination.Continuation; spec != nil && continuation(r, m.WorkflowID) == nil {
					r.Continuations = append(r.Continuations, chat.Continuation{ID: "continuation_" + m.WorkflowID, ParentWorkflow: m.WorkflowID, SourceSequence: m.Sequence, Spec: *spec, State: "queued"})
					changed = true
				}
			}
		}
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
			for i := range r.Handoffs {
				h := &r.Handoffs[i]
				if h.Open() && (h.ID == "work:"+w.ID || strings.HasPrefix(h.ID, "work:"+w.ID+"@")) && h.WaitingFor != w.ID {
					h.WaitingFor = w.ID
					changed = true
				}
			}
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
		if e.Kind == "result_available" && (r.State == "pending" || r.State == "blocked") {
			m := sourceMessages[e.SourceSequence]
			h := chat.Handoff{ID: e.ResultID, SourceSequence: e.SourceSequence, ReadyAt: e.At, Outcome: e.Outcome, Participant: m.Target, Owner: "chatgpt"}
			for _, j := range o.room.Conversations {
				if "reply:"+j.ID == e.ResultID {
					h.Participant = j.Assigned
					h.ResultSequence = j.AnswerSequence
					if h.Participant == "" && len(j.Requested) > 0 {
						h.Participant = j.Requested[0]
					}
				}
			}
			for _, answer := range o.messages {
				if answer.WorkflowID != "" && "work:"+answer.WorkflowID == e.ResultID && answer.Kind == chat.MessageText && answer.Author.ValidAgent() {
					h.ResultSequence = answer.Sequence
					h.Participant = answer.Author
				}
			}
			// A host can resume the same workflow after needs_attention or an
			// interruption. Its new result is a new obligation, with its own
			// delivery budget and dispatch key; preserve the prior audit record.
			var latest *chat.Handoff
			for i := range r.Handoffs {
				candidate := &r.Handoffs[i]
				if (candidate.ID == e.ResultID || strings.HasPrefix(candidate.ID, e.ResultID+"@")) && (latest == nil || candidate.ReadyAt.After(latest.ReadyAt)) {
					latest = candidate
				}
			}
			if latest == nil {
				r.Handoffs = append(r.Handoffs, h)
				changed = true
			} else if e.At.After(latest.ReadyAt) && (latest.Outcome != h.Outcome || latest.ResultSequence != h.ResultSequence) {
				if latest.Open() || latest.Resolution == "coordinator_reported_blocked" {
					latest.Resolution, latest.ResolvedAt = "superseded_by_result", e.At
				}
				h.ID = fmt.Sprintf("%s@%d", e.ResultID, e.At.UnixNano())
				e.ResultID = h.ID
				r.Handoffs = append(r.Handoffs, h)
				changed = true
			} else {
				e.ResultID = latest.ID
			}
			if strings.Contains(e.ResultID, "@") {
				e.ID += ":" + e.ResultID
			}
		}
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

	for i := range r.Handoffs {
		h := &r.Handoffs[i]
		if h.WaitingFor != "" {
			active := false
			if w, ok := o.room.Workflows[h.WaitingFor]; ok {
				active = w.State == chat.WorkflowQueued || w.State == chat.WorkflowActive || w.State == chat.WorkflowWaiting
			}
			for _, j := range o.room.Conversations {
				if j.ID == h.WaitingFor {
					active = !j.State.Terminal()
				}
			}
			if !active {
				h.WaitingFor = ""
				changed = true
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
func (o *Orchestrator) ReportCoordination(runID, eventID, kind, resultID, state, detail string, now time.Time, options ...chat.CoordinatorUpdate) (chat.CoordinationView, error) {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	update := chat.CoordinatorUpdate{}
	if len(options) > 0 {
		update = options[0]
	}
	if len(update.NextAction) > 1024 || len(update.Owner) > 128 || len(update.WaitingFor) > 128 || len(update.PanelID) > 128 {
		return chat.CoordinationView{}, fmt.Errorf("coordination update too long")
	}
	if update.Objective != nil && (len(update.Objective.Summary) > 1024 || len(update.Objective.Scope) > 2048 || len(update.Objective.CompletionCriteria) > 2048) {
		return chat.CoordinationView{}, fmt.Errorf("objective too long")
	}
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
	} else if kind != "notification_attempted" && kind != "host_accepted" && kind != "host_rejected" && kind != "host_unknown" && kind != "panel_status" {
		return chat.CoordinationView{}, fmt.Errorf("invalid observation kind")
	}
	previous := r.Clone()
	_, reconciled := o.reconcileCoordinationLocked(now)
	if resultID != "" {
		found := handoff(r, resultID) != nil
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
	if kind != "coordinator_report" && kind != "panel_status" && resultID == "" {
		o.room.Coordination = previous
		return chat.CoordinationView{}, fmt.Errorf("notification must identify a retained result")
	}
	if kind != "coordinator_report" && kind != "panel_status" && kind != "notification_attempted" {
		found := false
		for _, e := range r.Events {
			if e.ID == eventID && e.Kind == "notification_attempted" && e.ResultID == resultID {
				found = true
			}
		}
		if h := handoff(r, resultID); h != nil {
			for _, a := range h.Attempts {
				if a.ID == eventID {
					found = true
				}
			}
		}
		if !found {
			o.room.Coordination = previous
			return chat.CoordinationView{}, fmt.Errorf("notification attempt not recorded")
		}
	}
	for _, e := range r.Events {
		if e.ID == eventID && e.Kind == kind {
			if e.ResultID != resultID || e.Outcome != state || e.Detail != detail || !reflect.DeepEqual(e.Update, update) {
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
	if err := o.applyCoordinationReportLocked(r, eventID, kind, resultID, state, detail, update, now); err != nil {
		o.room.Coordination = previous
		return chat.CoordinationView{}, err
	}
	if kind != "panel_status" {
		r.Record(chat.CoordinationEvent{ID: eventID, Kind: kind, At: now, ResultID: resultID, Outcome: state, Detail: detail, Update: update})
	}
	if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
		o.room.Coordination = previous
		return chat.CoordinationView{}, err
	}
	v, _ := o.reconcileCoordinationLocked(now)
	return v, nil
}

func handoff(r *chat.CoordinationRun, id string) *chat.Handoff {
	if r == nil || id == "" {
		return nil
	}
	for i := range r.Handoffs {
		if r.Handoffs[i].ID == id {
			return &r.Handoffs[i]
		}
	}
	return nil
}
func continuation(r *chat.CoordinationRun, workflow string) *chat.Continuation {
	if r == nil {
		return nil
	}
	for i := range r.Continuations {
		if r.Continuations[i].ParentWorkflow == workflow {
			return &r.Continuations[i]
		}
	}
	return nil
}

func (o *Orchestrator) applyCoordinationReportLocked(r *chat.CoordinationRun, eventID, kind, resultID, state, detail string, update chat.CoordinatorUpdate, now time.Time) error {
	h := handoff(r, resultID)
	if kind == "panel_status" {
		switch update.Delivery {
		case "enabled", "manual", "hidden", "unsupported", "disconnected", "budget", "closed", "unknown":
		default:
			return fmt.Errorf("invalid panel delivery state")
		}
		if update.PanelID == "" {
			return fmt.Errorf("panel_id required")
		}
		for i := range r.Panels {
			if r.Panels[i].ID == update.PanelID {
				r.Panels[i].Mode, r.Panels[i].SeenAt = update.Delivery, now
				return nil
			}
		}
		if len(r.Panels) >= 8 {
			r.Panels = r.Panels[1:]
		}
		r.Panels = append(r.Panels, chat.CoordinationPanel{ID: update.PanelID, Mode: update.Delivery, SeenAt: now, StartedAt: now})
		return nil
	}
	if kind == "notification_attempted" && h != nil {
		for _, a := range h.Attempts {
			if a.ID == eventID {
				if a.Automatic != update.Automatic || a.PanelID != update.PanelID {
					return fmt.Errorf("notification claim ID reused by different request")
				}
				return nil
			}
		}
		if !h.Open() || r.State != "pending" {
			return fmt.Errorf("handoff already accounted for")
		}
		if now.Sub(r.LastNotificationAt) < 20*time.Second {
			return fmt.Errorf("another panel recently claimed a notification")
		}
		if update.Automatic {
			if !h.Due(now) {
				return fmt.Errorf("handoff notification is not due")
			}
			found := false
			for i := range r.Panels {
				p := &r.Panels[i]
				if p.ID == update.PanelID {
					found = true
					limits := chat.DefaultChatGPTLimits()
					if o.room.ChatGPT != nil && o.room.ChatGPT.Limits.FollowUps > 0 {
						limits = o.room.ChatGPT.Limits
					}
					if p.Mode != "enabled" || now.Sub(p.SeenAt) > 45*time.Second || p.Attempts >= limits.FollowUps || now.Sub(p.StartedAt) >= time.Duration(limits.FollowUpSeconds)*time.Second {
						return fmt.Errorf("panel unavailable or notification budget exhausted")
					}
					p.Attempts++
				}
			}
			if !found {
				return fmt.Errorf("register panel availability before automatic notification")
			}
		}
		h.Attempts = append(h.Attempts, chat.NotificationAttempt{PanelID: update.PanelID, ID: eventID, At: now, Outcome: "host_unknown", Automatic: update.Automatic})
		r.LastNotificationAt = now
	} else if strings.HasPrefix(kind, "host_") && h != nil {
		for i := range h.Attempts {
			if h.Attempts[i].ID == eventID {
				if h.Attempts[i].PanelID != update.PanelID || h.Attempts[i].Automatic != update.Automatic {
					return fmt.Errorf("notification outcome does not match its claim")
				}
				old := h.Attempts[i].Outcome
				if old != "host_unknown" && old != kind {
					return fmt.Errorf("notification outcome already recorded")
				}
				h.Attempts[i].Outcome = kind
			}
		}
	} else if kind == "coordinator_report" {
		if update.HandoffOnly && (h == nil || state == "stopped") {
			return fmt.Errorf("handoff_only requires a retained result and pending, blocked, or complete state")
		}
		if update.WaitingFor != "" {
			active := false
			if w, ok := o.room.Workflows[update.WaitingFor]; ok {
				active = w.State == chat.WorkflowQueued || w.State == chat.WorkflowActive || w.State == chat.WorkflowWaiting
			}
			for _, j := range o.room.Conversations {
				if j.ID == update.WaitingFor {
					active = !j.State.Terminal()
				}
			}
			if !active || h == nil {
				return fmt.Errorf("waiting_for must identify an active assignment for this handoff")
			}
		}
		if state == "complete" && !update.HandoffOnly {
			v, _ := o.reconcileCoordinationLocked(now)
			for _, c := range r.Continuations {
				if c.State == "queued" || c.State == "running" {
					return fmt.Errorf("cannot report complete while a registered review is pending")
				}
			}
			if v.PendingJobs > 0 {
				return fmt.Errorf("cannot report complete while assignments are pending")
			}
		}
		if !update.HandoffOnly {
			if r.State == "blocked" && state == "pending" {
				for i := range r.Handoffs {
					h := &r.Handoffs[i]
					if h.Resolution == "coordinator_reported_blocked" {
						h.Resolution, h.ResolvedAt = "", time.Time{}
					}
				}
			}
			r.State, r.Detail = state, detail
		}
		if state == "stopped" {
			o.cancelContinuationsLocked("Coordinator reported explicit stop")
		}
		if state == "complete" && !update.HandoffOnly {
			o.cancelContinuationsLocked("Coordinator reported objective complete")
		}
		if update.Objective != nil {
			obj := *update.Objective
			r.Objective = &obj
		}
		if update.NextAction != "" && !update.HandoffOnly {
			r.NextAction = update.NextAction
		}
		if update.Owner != "" && !update.HandoffOnly {
			r.Owner = update.Owner
		}
		if resultID != "" {
			r.AcknowledgedAt, r.AcknowledgedResult = now, resultID
		}
		h = handoff(r, resultID)
		if h != nil {
			if update.HandoffOnly && state == "pending" && h.Resolution == "coordinator_reported_blocked" {
				h.Resolution = ""
				h.ResolvedAt = time.Time{}
			}
			if h.AcknowledgedAt.IsZero() {
				h.AcknowledgedAt = now
			}
			h.NextAction = update.NextAction
			if h.NextAction == "" && (state == "pending" || state == "blocked") {
				h.NextAction = detail
			}
			h.WaitingFor = update.WaitingFor
			if update.Owner != "" {
				h.Owner = update.Owner
			}
		}
		if state == "complete" || state == "blocked" || state == "stopped" {
			for i := range r.Handoffs {
				h := &r.Handoffs[i]
				if h.Open() && (!update.HandoffOnly || h.ID == resultID) {
					h.Resolution, h.ResolvedAt = "coordinator_reported_"+state, now
				}
			}
		}
	}
	return nil
}
