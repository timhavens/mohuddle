package room

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestFollowUpsTerminalChangeWithoutTextHasDistinctClaim(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	_, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "panel_status", EventID: "panel", PanelID: "one", Delivery: "enabled"}, now)
	if err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.room.Workflows["work"] = chat.WorkflowRecord{ID: "work", State: chat.WorkflowNeedsAttention, UpdatedAt: now}
	o.mu.Unlock()
	state, messages := o.Snapshot()
	key := chat.NotificationKey(state, messages)
	u := chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "first", PanelID: "one", Revision: 1, Automatic: true, UpdateKey: key}
	if _, err = o.ObserveFollowUps(u, now); err != nil {
		t.Fatal(err)
	}
	u.EventID = "duplicate"
	if _, err = o.ObserveFollowUps(u, now.Add(21*time.Second)); err == nil {
		t.Fatal("same terminal result duplicated")
	}
	o.mu.Lock()
	o.room.Workflows["work"] = chat.WorkflowRecord{ID: "work", State: chat.WorkflowCompleted, UpdatedAt: now.Add(21 * time.Second)}
	o.mu.Unlock()
	state, messages = o.Snapshot()
	u.EventID = "completed"
	u.UpdateKey = chat.NotificationKey(state, messages)
	if u.UpdateKey == key {
		t.Fatal("new result without text cannot notify")
	}
	if _, err = o.ObserveFollowUps(u, now.Add(21*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffMissingHistoricalDispositionNeedsReconciliation(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	o.mu.Lock()
	o.room.Coordination = &chat.CoordinationRun{ID: "legacy", State: "pending", StartedAt: now, LastAssignmentAt: now, LastResultAt: now, StartSequence: 1}
	o.messages = append(o.messages, chat.Message{Sequence: 1, Author: chat.ChatGPT, RequestedReplies: []chat.Participant{chat.Codex}, CreatedAt: now})
	o.room.Conversations = append(o.room.Conversations, chat.ConversationJob{ID: "old", SourceSequence: 1, State: chat.ConversationAnswered, CompletedAt: &now})
	o.mu.Unlock()
	v, err := o.CoordinationStatus(now.Add(time.Hour))
	if err != nil || len(v.Handoffs) != 1 || !v.Handoffs[0].NeedsReconciliation || v.Handoffs[0].NotificationDue || v.Metrics.Stalled != 0 {
		t.Fatal(v, err)
	}
}

func TestHandoffHumanDecisionStartsFreshActionWindow(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	_, err := o.ReportCoordination(run, "wait", "coordinator_report", "reply:ready", "blocked", "Approve wording", now, chat.CoordinatorUpdate{HandoffOnly: true, WaitingOn: "human", Owner: "Tim"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.ReportCoordination(run, "ready", "coordinator_report", "reply:ready", "pending", "Decision received; review next", now.Add(time.Hour), chat.CoordinatorUpdate{HandoffOnly: true, WaitingOn: "coordinator", Owner: "chatgpt"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := o.CoordinationStatus(now.Add(time.Hour + time.Minute))
	if err != nil || v.ActionNeeded || v.Metrics.Stalled != 0 {
		t.Fatal("human decision time counted as stalled dispatch", v, err)
	}
	v, err = o.CoordinationStatus(now.Add(time.Hour + 3*time.Minute))
	if err != nil || !v.ActionNeeded || v.Metrics.Stalled != 1 {
		t.Fatal("genuine dispatch gap hidden", v, err)
	}
}

func TestFollowUpsDefaultSharedClaimsPauseAndRestart(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	state, _ := o.Snapshot()
	if !state.FollowUps.View(now, state.ChatGPT, nil).Enabled {
		t.Fatal("old rooms must default ON")
	}
	observe := func(u chat.FollowUpUpdate, at time.Time) chat.FollowUpView {
		t.Helper()
		v, e := o.ObserveFollowUps(u, at)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	for _, p := range []string{"one", "two"} {
		observe(chat.FollowUpUpdate{Stage: "panel_status", EventID: p, PanelID: p, Delivery: "enabled"}, now)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, p := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: p, PanelID: p, Automatic: true, Revision: 1, Through: 10}, now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d panels claimed one update", winners)
	}
	v := observe(chat.FollowUpUpdate{Stage: "pause", EventID: "pause", Revision: 1}, now)
	if v.Enabled || v.Revision != 2 || v.Remaining != 31 {
		t.Fatal(v)
	}
	// A stale panel heartbeat and reconnect cannot undo an explicit pause.
	observe(chat.FollowUpUpdate{Stage: "panel_status", EventID: "old", PanelID: "one", Delivery: "enabled"}, now.Add(time.Minute))
	if _, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "late", PanelID: "one", Automatic: true, Revision: 1, Through: 11}, now.Add(time.Minute)); err == nil {
		t.Fatal("stale panel sent after pause")
	}
	if _, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "resume", EventID: "stale-control", Revision: 1}, now); err == nil {
		t.Fatal("stale control overwrote pause")
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
	connectChatGPT(restored)
	v = loaded.FollowUps.View(now, loaded.ChatGPT, loaded.Coordination)
	if v.Enabled || v.Remaining != 31 {
		t.Fatal("restart lost policy or consumption", v)
	}
	v, err = restored.ObserveFollowUps(chat.FollowUpUpdate{Stage: "resume", EventID: "resume", Revision: 2}, now.Add(time.Minute))
	if err != nil || !v.Enabled || v.Remaining != 31 {
		t.Fatal(v, err)
	}
	_, err = restored.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "duplicate-sequence", PanelID: "one", Automatic: true, Revision: 3, Through: 10}, now.Add(time.Minute))
	if err == nil {
		t.Fatal("reopening duplicated old update")
	}
}

func TestFollowUpsShareAllowanceWithHandoffsAndRenewExplicitly(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	o.mu.Lock()
	o.room.ChatGPT.Limits = chat.ChatGPTLimits{Exchanges: 10, FollowUps: 2, FollowUpSeconds: 60, RepeatedRequests: 3}
	o.mu.Unlock()
	panel := chat.CoordinatorUpdate{PanelID: "one", Delivery: "enabled"}
	if _, err := o.ReportCoordination(run, "panel", "panel_status", "", "", "", now, panel); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ReportCoordination(run, "handoff", "notification_attempted", "reply:ready", "", "", now, chat.CoordinatorUpdate{PanelID: "one", Automatic: true}); err != nil {
		t.Fatal(err)
	}
	v, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "general", PanelID: "one", Automatic: true, Revision: 1, Through: 2}, now.Add(21*time.Second))
	if err != nil || v.Remaining != 0 {
		t.Fatal(v, err)
	}
	for _, stage := range []string{"panel_status", "resume"} {
		u := chat.FollowUpUpdate{Stage: stage, EventID: stage, PanelID: "two", Delivery: "enabled", Revision: 1}
		v, err = o.ObserveFollowUps(u, now.Add(42*time.Second))
		if err != nil || v.Remaining != 0 {
			t.Fatal("panel or resume replenished allowance", v, err)
		}
	}
	if _, err = o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "over-budget", PanelID: "two", Automatic: true, Revision: v.Revision, Through: 3}, now.Add(42*time.Second)); err == nil {
		t.Fatal("shared budget bypassed")
	}
	v, err = o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "manual", PanelID: "two", Through: 3}, now.Add(42*time.Second))
	if err != nil || v.Remaining != 0 {
		t.Fatal("manual result collection unavailable", v, err)
	}
	v, err = o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "renew", EventID: "renew", Revision: v.Revision}, now.Add(2*time.Minute))
	if err != nil || v.Remaining != 2 || v.SecondsRemaining != 60 {
		t.Fatal(v, err)
	}
	// Retrying renewal cannot reset the window a second time.
	retry, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "renew", EventID: "renew", Revision: v.Revision - 1}, now.Add(3*time.Minute))
	if err != nil || retry.SecondsRemaining != 0 {
		t.Fatal(retry, err)
	}
}

func TestFollowUpsRejectUnavailablePanelsAndPreserveHostStop(t *testing.T) {
	for _, mode := range []string{"hidden", "unsupported", "disconnected", "closed", "stale"} {
		t.Run(mode, func(t *testing.T) {
			o, _, _ := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			now := time.Now().UTC()
			delivery := mode
			if mode == "stale" {
				delivery = "enabled"
			}
			_, err := o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "panel_status", EventID: "panel", PanelID: "one", Delivery: delivery}, now)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale" {
				now = now.Add(46 * time.Second)
			}
			if _, err = o.ObserveFollowUps(chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "claim", PanelID: "one", Automatic: true, Revision: 1, Through: 1}, now); err == nil {
				t.Fatal("unavailable panel claimed")
			}
		})
	}
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	o.Stop()
	state, messages := o.Snapshot()
	restored, err := New(state, messages, o.store, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	connectChatGPT(restored)
	state, _ = restored.Snapshot()
	if !state.ChatGPT.Paused || !state.FollowUps.HostPaused {
		t.Fatal("restart cleared explicit host stop")
	}
}

func TestHandoffCompletedHistoryDoesNotResurrectAfterNewAssignment(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			o, _, _ := newTestOrchestrator(t)
			defer o.Close()
			now := time.Now().UTC()
			o.ControlCoordination("start", now)
			o.mu.Lock()
			r := o.room.Coordination
			o.messages = append(o.messages, chat.Message{Sequence: 1, Author: chat.ChatGPT, RequestedReplies: []chat.Participant{chat.Codex}, Kind: chat.MessageText, CreatedAt: now})
			ready := now.Add(time.Minute)
			o.room.Conversations = append(o.room.Conversations, chat.ConversationJob{ID: "old", SourceSequence: 1, Assigned: chat.Codex, State: chat.ConversationAnswered, CompletedAt: &ready, AnswerSequence: 2})
			o.mu.Unlock()
			v, err := o.CoordinationStatus(ready)
			if err != nil || len(v.Handoffs) != 1 {
				t.Fatal(v, err)
			}
			_, err = o.ReportCoordination(r.ID, "done", "coordinator_report", "reply:old", "complete", "Finished objective", now.Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			o.mu.Lock()
			if legacy {
				r.LedgerVersion = 0
				r.CompletedThrough = time.Time{}
				r.Handoffs = nil
			}
			o.mu.Unlock()
			// Reconcile migration before evicting legacy evidence, like loading a room.
			if _, err = o.CoordinationStatus(now.Add(3 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			o.mu.Lock()
			for i := 0; i < 250; i++ {
				r.Record(chat.CoordinationEvent{ID: fmt.Sprint(i), Kind: "test"})
			}
			o.messages = append(o.messages, chat.Message{Sequence: 3, Author: chat.ChatGPT, RequestedReplies: []chat.Participant{chat.Claude}, Kind: chat.MessageText, CreatedAt: now.Add(4 * time.Minute)})
			o.mu.Unlock()
			v, err = o.CoordinationStatus(now.Add(time.Hour))
			if err != nil || v.Metrics.Outstanding != 0 || v.State != "pending" {
				t.Fatalf("historical result resurrected: %+v %v", v, err)
			}
		})
	}
}

func TestHandoffHumanWaitAndLinkedAssignmentLeaveOtherBranchesBlocked(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	run := readyHandoff(t, o, now)
	o.mu.Lock()
	o.room.Coordination.Handoffs = append(o.room.Coordination.Handoffs, chat.Handoff{ID: "reply:other", ReadyAt: now, Owner: "Tim", WaitingOn: "human"})
	o.mu.Unlock()
	v, err := o.ReportCoordination(run, "decision", "coordinator_report", "reply:ready", "blocked", "Approve wording", now, chat.CoordinatorUpdate{HandoffOnly: true, WaitingOn: "human", Owner: "Tim"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = o.CoordinationStatus(now.Add(time.Hour))
	if err != nil || v.Metrics.Stalled != 0 || v.ActionNeeded || !v.Handoffs[0].Open() {
		t.Fatal("human wait marked as stall", v, err)
	}
	_, err = o.ReportCoordination(run, "pending", "coordinator_report", "", "pending", "Decision received; dispatch", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := o.ScheduleChatGPT(chat.ChatGPTAssignment{Text: "Apply approved wording", Participants: []chat.Participant{chat.Claude}, Route: chat.RouteMetadata{MessageID: "next"}, Coordination: &chat.CoordinationDispatch{RunID: run, HandoffID: "reply:ready"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err = o.CoordinationStatus(now.Add(2 * time.Minute))
	if err != nil || v.Handoffs[0].AssignmentSequence != m.Sequence || v.Handoffs[0].Open() || v.Handoffs[1].WaitingOn != "human" || v.Handoffs[1].NotificationDue {
		t.Fatal(v, err)
	}
}
