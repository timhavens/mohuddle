package room

import (
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestCoordinationStopSurvivesRenewalAndPersistence(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	now := time.Now().UTC()
	v, err := o.ControlCoordination("start", now)
	if err != nil {
		t.Fatal(err)
	}
	o.Stop()
	o.UpdateChatGPTState(chat.ChatGPTState{})
	connectChatGPT(o)
	o.ResumeChatGPT()
	state, _ := o.Snapshot()
	if state.Coordination.State != "stopped" {
		t.Fatal("access changes revived stopped run")
	}
	loaded, err := o.store.(interface {
		LoadRoom(string) (chat.Room, error)
	}).LoadRoom(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Coordination == nil || loaded.Coordination.State != "stopped" {
		t.Fatal("stop not persisted")
	}
	if _, _, err := o.PublishChatGPT("read only", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "stale"}); err == nil {
		t.Fatal("stopped run dispatched")
	}
	if _, err := o.ReportCoordination(v.ID, "stale", "coordinator_report", "", "pending", "continue", now); err == nil {
		t.Fatal("remote report resumed stop")
	}
	next, err := o.ControlCoordination("resume", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == v.ID {
		t.Fatal("resume did not invalidate stale reports")
	}
	if _, err := o.ReportCoordination(v.ID, "late", "coordinator_report", "", "pending", "continue", now); err == nil {
		t.Fatal("old identity accepted")
	}
}

func TestCoordinationReconcilesResultsAndSeparatesDeliveryFromAcknowledgement(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	now := time.Now().UTC()
	v, err := o.ControlCoordination("start", now)
	if err != nil {
		t.Fatal(err)
	}
	done := now.Add(time.Minute)
	o.mu.Lock()
	o.messages = append(o.messages, chat.Message{Sequence: o.nextSequence, Author: chat.ChatGPT, RequestedReplies: []chat.Participant{chat.Codex}, CreatedAt: now})
	o.room.Conversations = append(o.room.Conversations, chat.ConversationJob{ID: "demo", SourceSequence: o.nextSequence, State: chat.ConversationAnswered, CompletedAt: &done})
	o.mu.Unlock()
	v, err = o.CoordinationStatus(done)
	if err != nil {
		t.Fatal(err)
	}
	if v.LastSuccessAt != done || v.LastResultAt != done {
		t.Fatal("result not reconciled")
	}
	for _, kind := range []string{"notification_attempted", "host_accepted"} {
		v, err = o.ReportCoordination(v.ID, "notification", kind, "reply:demo", "", "", done)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !v.AcknowledgedAt.IsZero() {
		t.Fatal("transport accepted counted as coordinator acknowledgement")
	}
	v, err = o.ReportCoordination(v.ID, "ack", "coordinator_report", "reply:demo", "pending", "Review the completed result", done.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if v.AcknowledgedResult != "reply:demo" {
		t.Fatal("explicit acknowledgement missing")
	}
	before := len(v.Events)
	v, err = o.ReportCoordination(v.ID, "ack", "coordinator_report", "reply:demo", "pending", "Review the completed result", done.Add(2*time.Minute))
	if err != nil || len(v.Events) != before {
		t.Fatal("retry duplicated report", err)
	}
	if _, err = o.ReportCoordination(v.ID, "ack", "coordinator_report", "reply:demo", "pending", "Different action", done); err == nil {
		t.Fatal("changed retry accepted")
	}
	v, err = o.CoordinationStatus(done.Add(11 * time.Minute))
	if err != nil || !v.ActionNeeded {
		t.Fatal("acknowledgement hid stalled coordination", err)
	}
	state, _ := o.Snapshot()
	if len(state.Conversations) != 1 {
		t.Fatal("monitor dispatched a new job")
	}
}
