package room

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func readyHandoff(t *testing.T, o *Orchestrator, now time.Time) string {
	t.Helper()
	v, err := o.ControlCoordination("start", now)
	if err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.room.Coordination.Handoffs = append(o.room.Coordination.Handoffs, chat.Handoff{ID: "reply:ready", SourceSequence: 1, ReadyAt: now, Owner: "chatgpt", Participant: chat.Codex})
	o.mu.Unlock()
	return v.ID
}

func TestHandoffClaimsRetainOutstandingWorkAndBoundRecovery(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	panel := chat.CoordinatorUpdate{PanelID: "panel-one", Delivery: "enabled"}
	report := func(id, kind string, at time.Time, u chat.CoordinatorUpdate) (chat.CoordinationView, error) {
		result := "reply:ready"
		if kind == "panel_status" {
			result = ""
		}
		return o.ReportCoordination(run, id, kind, result, "", "", at, u)
	}
	if _, err := report("panel-one", "panel_status", now, panel); err != nil {
		t.Fatal(err)
	}
	auto := chat.CoordinatorUpdate{Automatic: true, PanelID: "panel-one"}
	for i, seconds := range []int{0, 60, 180} {
		at := now.Add(time.Duration(seconds) * time.Second)
		if _, err := report("panel-one", "panel_status", at, panel); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("attempt%d", i)
		if _, err := report(id, "notification_attempted", at, auto); err != nil {
			t.Fatal(err)
		}
		if _, err := report("competing", "notification_attempted", at, auto); err == nil {
			t.Fatal("competing claim accepted")
		}
		v, err := report(id, "host_accepted", at, auto)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Handoffs[0].Open() || !v.AcknowledgedAt.IsZero() {
			t.Fatal("delivery resolved or acknowledged handoff")
		}
	}
	if _, err := report("fourth", "notification_attempted", now.Add(4*time.Minute), auto); err == nil {
		t.Fatal("retry budget ignored")
	}
	v, err := o.ReportCoordination(run, "ack", "coordinator_report", "reply:ready", "pending", "Assign the review", now.Add(4*time.Minute))
	if err != nil || !v.ActionNeeded || !v.Handoffs[0].Open() {
		t.Fatalf("acknowledgment hid promised dispatch: %+v %v", v, err)
	}
	// Evict event history; the real obligation and attempt count must survive.
	o.mu.Lock()
	for i := 0; i < 250; i++ {
		o.room.Coordination.Record(chat.CoordinationEvent{ID: fmt.Sprint(i), Kind: "test"})
	}
	o.mu.Unlock()
	v, err = o.CoordinationStatus(now.Add(38 * time.Minute))
	if err != nil || !v.ActionNeeded || len(v.Handoffs[0].Attempts) != 3 {
		t.Fatal("38-minute gap lost handoff", err)
	}
	if v.Metrics.NotificationAttempts != 3 || v.Metrics.Acknowledged != 1 || v.Metrics.Outstanding != 1 {
		t.Fatal(v.Metrics)
	}
	if !strings.Contains(v.RecoveryStatus, "exhausted") {
		t.Fatal("exhausted recovery still appears available", v.RecoveryStatus)
	}
	// Link a real assignment, not a textual promise.
	m, created, err := o.ScheduleChatGPT(chat.ChatGPTAssignment{Text: "Review result", Participants: []chat.Participant{chat.Claude}, Route: chat.RouteMetadata{MessageID: "review"}, Coordination: &chat.CoordinationDispatch{RunID: run, HandoffID: "reply:ready"}})
	if err != nil || !created {
		t.Fatal(err)
	}
	v, err = o.CoordinationStatus(now.Add(39 * time.Minute))
	if err != nil || v.Handoffs[0].AssignmentSequence != m.Sequence || v.Handoffs[0].Open() {
		t.Fatal("accepted dispatch did not resolve handoff", err)
	}
	if _, _, err := o.ScheduleChatGPT(chat.ChatGPTAssignment{Text: "Duplicate review", Participants: []chat.Participant{chat.Claude}, Route: chat.RouteMetadata{MessageID: "different"}, Coordination: &chat.CoordinationDispatch{RunID: run, HandoffID: "reply:ready"}}); err == nil {
		t.Fatal("handoff dispatched twice")
	}
}

func TestHandoffHostRejectionAndStoppedRunDoNotRetry(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	for _, kind := range []string{"notification_attempted", "host_rejected"} {
		if _, err := o.ReportCoordination(run, "reject", kind, "reply:ready", "", "", now); err != nil {
			t.Fatal(err)
		}
	}
	v, err := o.CoordinationStatus(now.Add(time.Minute))
	if err != nil || v.Handoffs[0].NotificationDue {
		t.Fatal("rejection was retried")
	}
	if _, err := o.ControlCoordination("stop", now); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ReportCoordination(run, "late", "notification_attempted", "reply:ready", "", "", now.Add(time.Hour)); err == nil {
		t.Fatal("stopped run accepted notification")
	}
}

func TestHandoffIndependentResultIsVisibleWhileOtherWorkRuns(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	readyHandoff(t, o, now)
	o.mu.Lock()
	o.messages = append(o.messages, chat.Message{Sequence: 2, Author: chat.ChatGPT, RequestedReplies: []chat.Participant{chat.Claude}, CreatedAt: now})
	o.room.Conversations = append(o.room.Conversations, chat.ConversationJob{ID: "slow", SourceSequence: 2, State: chat.ConversationAnswering})
	o.mu.Unlock()
	v, err := o.CoordinationStatus(now.Add(3 * time.Minute))
	if err != nil || !v.ActionNeeded || v.PendingJobs != 1 || !v.Handoffs[0].NotificationDue {
		t.Fatalf("parallel work hid ready handoff: %+v %v", v, err)
	}
}

func TestRegisteredContinuationRunsReadOnlyAfterPassiveLeaseExpiry(t *testing.T) {
	o, writer, reviewer := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	run, err := o.ControlCoordination("start", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	writer.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{Text: "Exact parent result XYZ", Done: true}, nil
	}
	reviewed := make(chan agent.TurnRequest, 1)
	reviewer.run = func(_ context.Context, _ int, r agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		reviewed <- r
		return agent.TurnResult{Text: "Reviewed XYZ", Done: true}, nil
	}
	task := chat.ChatGPTAssignment{Kind: "work", Text: "Produce XYZ", Target: chat.Codex, Route: chat.RouteMetadata{MessageID: "parent"}, Coordination: &chat.CoordinationDispatch{RunID: run.ID, Continuation: &chat.ContinuationSpec{Target: chat.Claude, Text: "Verify XYZ"}}}
	m, created, err := o.ScheduleChatGPT(task)
	if err != nil || !created {
		t.Fatal(err)
	}
	<-started
	o.UpdateChatGPTState(chat.ChatGPTState{Enabled: true, Connected: false, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(-time.Minute)})
	close(release)
	select {
	case r := <-reviewed:
		if r.Settings.Permissions != chat.PermissionReadOnly || len(r.WriteRoots) > 0 || !strings.Contains(r.Prompt, "Exact parent result XYZ") {
			t.Fatal("review lost result or read-only ceiling", r.Settings.Permissions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("review required a new ChatGPT turn")
	}
	waitChatGPTReplies(t, o, 1)
	waitForCondition(t, 3*time.Second, func() bool { v, e := o.CoordinationStatus(time.Now()); return e == nil && len(v.Handoffs) > 1 })
	o.refreshCoordination()
	state, _ := o.Snapshot()
	c := continuation(state.Coordination, m.WorkflowID)
	if c == nil || c.State != "completed" || reviewer.callCount() != 1 {
		t.Fatalf("continuation %+v calls=%d", c, reviewer.callCount())
	}
	// Rejoining and retrying the parent is idempotent.
	connectChatGPT(o)
	if _, created, err := o.ScheduleChatGPT(task); err != nil || created {
		t.Fatal("parent retry duplicated work", err)
	}
	o.refreshCoordination()
	if reviewer.callCount() != 1 {
		t.Fatal("review duplicated")
	}
	task.Coordination.Continuation.Text = "Different task"
	if _, _, err := o.ScheduleChatGPT(task); err == nil {
		t.Fatal("changed continuation reused operation")
	}
}

func TestContinuationCancelledOnAccessRenewalAndRestart(t *testing.T) {
	for _, mode := range []string{"revoke", "leave", "stop", "restart"} {
		t.Run(mode, func(t *testing.T) {
			o, _, _ := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			run, err := o.ControlCoordination("start", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			o.mu.Lock()
			o.room.Coordination.Continuations = []chat.Continuation{{ID: "saved", ParentWorkflow: "parent", State: "queued", Spec: chat.ContinuationSpec{Target: chat.Claude, Text: "Review"}}}
			o.mu.Unlock()
			switch mode {
			case "revoke":
				o.UpdateChatGPTState(chat.ChatGPTState{})
				connectChatGPT(o)
			case "leave":
				o.EndChatGPTParticipation(chat.ReasonChatGPTLeft)
				connectChatGPT(o)
			case "stop":
				o.Stop()
				_, err = o.ControlCoordination("resume", time.Now())
				if err != nil {
					t.Fatal(err)
				}
			case "restart":
				state, messages := o.Snapshot()
				restored, err := New(state, messages, o.store, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				connectChatGPT(restored)
				o = restored
			}
			state, _ := o.Snapshot()
			if state.Coordination.Continuations[0].State != "cancelled" {
				t.Fatal(mode, run.ID, "revived continuation")
			}
		})
	}
}

func TestHandoffScopedBlockerDoesNotPauseIndependentWork(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now()
	run := readyHandoff(t, o, now)
	o.mu.Lock()
	o.room.Coordination.Handoffs = append(o.room.Coordination.Handoffs, chat.Handoff{ID: "reply:other", SourceSequence: 2, ReadyAt: now, Owner: "chatgpt"})
	o.mu.Unlock()
	v, err := o.ReportCoordination(run, "block-one", "coordinator_report", "reply:ready", "blocked", "Need product decision", now, chat.CoordinatorUpdate{HandoffOnly: true, Owner: "human"})
	if err != nil || v.State != "pending" || v.Handoffs[0].Open() || !v.Handoffs[1].NotificationDue {
		t.Fatalf("branch blocker stopped independent work: %+v %v", v, err)
	}
}

func TestHandoffPersistenceSurvivesRestartWithoutReopeningCompletedRun(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now()
	run := readyHandoff(t, o, now)
	if _, err := o.ReportCoordination(run, "ack", "coordinator_report", "reply:ready", "pending", "Review next", now); err != nil {
		t.Fatal(err)
	}
	state, messages := o.Snapshot()
	loaded, err := o.store.(interface {
		LoadRoom(string) (chat.Room, error)
	}).LoadRoom(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(loaded, messages, o.store, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	v, err := restored.CoordinationStatus(now.Add(38 * time.Minute))
	if err != nil || !v.ActionNeeded || v.Handoffs[0].NextAction != "Review next" {
		t.Fatal("restart lost outstanding action", err)
	}
	if _, err := restored.ReportCoordination(run, "complete", "coordinator_report", "reply:ready", "complete", "Objective done", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, err = restored.CoordinationStatus(now.Add(2 * time.Hour))
	if err != nil || v.State != "complete" || v.ActionNeeded {
		t.Fatal("completed run revived", err)
	}
}

func TestContinuationRestartReconcilesSavedDispatchBeforeCancellation(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(fmt.Sprint(answered), func(t *testing.T) {
			o, _, _ := newTestOrchestrator(t)
			defer o.Close()
			now := time.Now().UTC()
			readyHandoff(t, o, now)
			o.mu.Lock()
			o.room.Coordination.Continuations = []chat.Continuation{{ID: "saved-review", ParentWorkflow: "parent", State: "queued"}}
			o.messages = append(o.messages, chat.Message{Sequence: 10, Author: chat.ChatGPT, Kind: chat.MessageText, Text: "Review", CreatedAt: now, Route: &chat.RouteMetadata{MessageID: "saved-review"}})
			if answered {
				o.room.Conversations = append(o.room.Conversations, chat.ConversationJob{ID: "review", SourceSequence: 10, State: chat.ConversationAnswered, CompletedAt: &now})
			}
			o.mu.Unlock()
			state, messages := o.Snapshot()
			reviewer := &fakeAgent{participant: chat.Claude}
			restored, err := New(state, messages, o.store, reviewer)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			connectChatGPT(restored)
			restored.refreshCoordination()
			state, _ = restored.Snapshot()
			c := state.Coordination.Continuations[0]
			want := "blocked"
			if answered {
				want = "completed"
			}
			if c.State != want || c.AssignmentSequence != 10 || reviewer.callCount() != 0 {
				t.Fatalf("saved dispatch lost or duplicated: %+v calls=%d", c, reviewer.callCount())
			}
		})
	}
}

func TestContinuationPauseFailureAndMissingEvidenceDoNotDispatch(t *testing.T) {
	for _, mode := range []string{"pause", "failure", "missing", "oversized", "expired"} {
		t.Run(mode, func(t *testing.T) {
			o, _, reviewer := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			now := time.Now().UTC()
			readyHandoff(t, o, now)
			o.mu.Lock()
			o.room.Coordination.Continuations = []chat.Continuation{{ID: "review", ParentWorkflow: "parent", SourceSequence: 1, State: "queued", Spec: chat.ContinuationSpec{Target: chat.Claude, Text: "Verify result"}}}
			o.room.Coordination.Handoffs = []chat.Handoff{{ID: "work:parent", SourceSequence: 1, ResultSequence: 2, ReadyAt: now, Owner: "chatgpt"}}
			o.room.Workflows["parent"] = chat.WorkflowRecord{ID: "parent", State: chat.WorkflowCompleted}
			switch mode {
			case "pause":
				o.room.ChatGPT.Paused = true
			case "failure":
				o.room.Workflows["parent"] = chat.WorkflowRecord{ID: "parent", State: chat.WorkflowNeedsAttention}
			case "oversized":
				o.messages = append(o.messages, chat.Message{Sequence: 2, Author: chat.Codex, Text: strings.Repeat("X", 16001)})
			case "expired":
				o.room.ChatGPT.ExpiresAt = now.Add(-time.Minute)
			}
			o.mu.Unlock()
			o.advanceContinuation("parent", now)
			state, _ := o.Snapshot()
			c := state.Coordination.Continuations[0]
			want := "blocked"
			if mode == "pause" {
				want = "queued"
			} else if mode == "expired" {
				want = "cancelled"
			}
			if c.State != want || c.Reason == "" || reviewer.callCount() != 0 {
				t.Fatalf("%s: %+v calls=%d", mode, c, reviewer.callCount())
			}
		})
	}
}

func TestHandoffClearingGlobalBlockerRestoresOutstandingAction(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	if _, err := o.ReportCoordination(run, "block", "coordinator_report", "reply:ready", "blocked", "Need decision", now); err != nil {
		t.Fatal(err)
	}
	v, err := o.ReportCoordination(run, "unblock", "coordinator_report", "reply:ready", "pending", "Decision received; review next", now.Add(time.Minute))
	if err != nil || !v.Handoffs[0].Open() || !v.Handoffs[0].NotificationDue {
		t.Fatalf("clearing blocker lost action: %+v %v", v, err)
	}
}

func TestHandoffResumedWorkflowCreatesNewResultObligation(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	if _, err := o.ControlCoordination("start", now); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.messages = append(o.messages, chat.Message{Sequence: 1, Author: chat.User, WorkflowID: "recovering", Kind: chat.MessageText, Text: "Do the work", CreatedAt: now})
	o.room.Workflows["recovering"] = chat.WorkflowRecord{ID: "recovering", State: chat.WorkflowNeedsAttention, SourceSequences: []uint64{1}, UpdatedAt: now}
	o.mu.Unlock()
	v, err := o.CoordinationStatus(now)
	if err != nil || len(v.Handoffs) != 1 || v.Handoffs[0].Outcome != "needs_attention" {
		t.Fatalf("initial failure not visible: %+v %v", v, err)
	}
	o.mu.Lock()
	w := o.room.Workflows["recovering"]
	w.State, w.UpdatedAt = chat.WorkflowActive, now.Add(time.Minute)
	o.room.Workflows[w.ID] = w
	o.mu.Unlock()
	v, err = o.CoordinationStatus(now.Add(time.Minute))
	if err != nil || v.Handoffs[0].NotificationDue || v.Handoffs[0].WaitingFor != w.ID {
		t.Fatal("resumed work still notifies as ready", err)
	}
	o.mu.Lock()
	w.State, w.UpdatedAt = chat.WorkflowCompleted, now.Add(2*time.Minute)
	w.CompletedAt = &w.UpdatedAt
	o.room.Workflows[w.ID] = w
	o.messages = append(o.messages, chat.Message{Sequence: 2, Author: chat.Codex, WorkflowID: w.ID, Kind: chat.MessageText, Text: "Recovered actual result", CreatedAt: w.UpdatedAt})
	o.mu.Unlock()
	v, err = o.CoordinationStatus(now.Add(2 * time.Minute))
	if err != nil || len(v.Handoffs) != 2 || v.Handoffs[0].Resolution != "superseded_by_result" || v.Handoffs[1].ResultSequence != 2 || !v.Handoffs[1].NotificationDue {
		t.Fatalf("new result lost or obsolete blocker retained: %+v %v", v, err)
	}
	v, err = o.CoordinationStatus(now.Add(3 * time.Minute))
	if err != nil || len(v.Handoffs) != 2 {
		t.Fatal("reconciliation duplicated resumed result", err)
	}
}
