//go:build !windows

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
)

type handoffPeer struct{ participant chat.Participant }

func (p handoffPeer) Participant() chat.Participant { return p.participant }
func (p handoffPeer) Close() error                  { return nil }
func (p handoffPeer) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: "Completed assigned work", Done: true}, nil
}

// Exercise the real API and scheduler without needing an OS listener.
func handoffService(t *testing.T) (*Service, *room.Orchestrator, *Session) {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, st, handoffPeer{chat.Codex}, handoffPeer{chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	o.ConfigureTemporaryAgents(nil)
	creds, err := LoadOrCreateCredentials(CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(*creds, o)
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureChatGPT(filepath.Join(root, "unused.sock"), filepath.Join(root, "chatgpt.json"), nil)
	path, err := s.EnableChatGPT(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var conn ChatGPTConnection
	if err := json.Unmarshal(data, &conn); err != nil {
		t.Fatal(err)
	}
	session, err := s.Authenticate(HelloRequest{ClientID: "test", Token: conn.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.RevokeChatGPT(); _ = o.Close() })
	return s, o, session
}

func TestHandoffAPIJoinRecoveryAndLinkedDispatch(t *testing.T) {
	s, _, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	if v.InstructionVersion == "" || !strings.Contains(v.Usage, "Coordinator procedure") {
		t.Fatal("join did not deliver full guidance")
	}
	run, err := s.ControlCoordination("start")
	if err != nil {
		t.Fatal(err)
	}
	r := chatGPTCall(t, s, session, "chatgpt.coordinator_report", CoordinatorReportRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "objective", State: "pending", Detail: "Implement then review", Objective: &chat.CoordinationObjective{Summary: "Fix defect", Scope: "This defect only", CompletionCriteria: "Fix independently reviewed"}, NextAction: "Implement", Owner: "chatgpt"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	r = chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "first", Text: "Investigate", RequestReplies: []chat.Participant{chat.Codex}})
	if !r.OK {
		t.Fatal(r.Error)
	}
	v = awaitChatGPTReplies(t, s, session, v.ParticipationID)
	if v.Coordination == nil || len(v.Coordination.Handoffs) != 1 || v.ActionRequired == "" {
		t.Fatal("ready handoff missing", v.Coordination)
	}
	h := v.Coordination.Handoffs[0]
	// Simulate a new participation; the objective and handoff remain visible.
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Minute)
	s.chatgptMu.Unlock()
	v = joinChatGPT(t, s, session)
	if v.Coordination.Objective.Summary != "Fix defect" || v.Coordination.Handoffs[0].ID != h.ID {
		t.Fatal("reconnect lost checkpoint")
	}
	r = chatGPTCall(t, s, session, "chatgpt.coordinator_report", CoordinatorReportRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "ack", ResultID: h.ID, State: "pending", Detail: "Review next", NextAction: "Review", Owner: "chatgpt"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	if r.Result.(chat.CoordinationView).Handoffs[0].Resolution != "" {
		t.Fatal("promise resolved handoff")
	}
	input := ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "review", Text: "Review completed evidence", RequestReplies: []chat.Participant{chat.Claude}, Coordination: &chat.CoordinationDispatch{RunID: run.ID, HandoffID: h.ID}}
	r = chatGPTCall(t, s, session, "chatgpt.publish", input)
	if !r.OK {
		t.Fatal(r.Error)
	}
	if r = chatGPTCall(t, s, session, "chatgpt.publish", input); !r.OK || r.Result.(map[string]any)["duplicate"] != true {
		t.Fatal("retry not idempotent", r.Error)
	}
	v = awaitChatGPTReplies(t, s, session, v.ParticipationID)
	if v.Coordination.Handoffs[0].Resolution != "assignment_accepted" {
		t.Fatal("accepted assignment unlinked")
	}
}

func TestHandoffAPIContinuationReservesBudgetAndRequiresExactRetry(t *testing.T) {
	s, _, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	run, err := s.ControlCoordination("start")
	if err != nil {
		t.Fatal(err)
	}
	input := ChatGPTWorkRequest{ParticipationID: v.ParticipationID, OperationID: "writer", Target: chat.Codex, Text: "Prepare the artifact", Coordination: &chat.CoordinationDispatch{RunID: run.ID, Continuation: &chat.ContinuationSpec{Target: chat.Claude, Text: "Review the actual artifact"}}}
	s.chatgptMu.Lock()
	s.chatgpt.exchanges = s.chatgpt.effectiveLimits().Exchanges - 1
	s.chatgptMu.Unlock()
	r := chatGPTCall(t, s, session, "chatgpt.request_work", input)
	if r.OK || r.Error.Code != "exchange_limit" {
		t.Fatal("unbudgeted continuation accepted", r.Error)
	}
	if err := s.ResumeChatGPT(); err != nil {
		t.Fatal(err)
	}
	r = chatGPTCall(t, s, session, "chatgpt.request_work", input)
	if !r.OK {
		t.Fatal(r.Error)
	}
	if r.Result.(map[string]any)["exchanges_remaining"] != chat.DefaultChatGPTLimits().Exchanges-2 {
		t.Fatal("review exchange not reserved")
	}
	if c, ok := r.Result.(map[string]any)["continuation"].(chat.Continuation); !ok || c.Spec.Target != chat.Claude || c.Spec.Text != input.Coordination.Continuation.Text {
		t.Fatal("receipt omitted the registered review")
	}
	r = chatGPTCall(t, s, session, "chatgpt.request_work", input)
	if !r.OK || r.Result.(map[string]any)["duplicate"] != true {
		t.Fatal(r.Error)
	}
	input.Coordination.Continuation.Text = "Different review"
	if r = chatGPTCall(t, s, session, "chatgpt.request_work", input); r.OK {
		t.Fatal("changed continuation accepted")
	}
}

func TestHandoffAPIClaimRequiresPanelAndRejectsCompetingAttempt(t *testing.T) {
	s, _, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	run, err := s.ControlCoordination("start")
	if err != nil {
		t.Fatal(err)
	}
	r := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "result", Text: "Read this", RequestReplies: []chat.Participant{chat.Codex}})
	if !r.OK {
		t.Fatal(r.Error)
	}
	v = awaitChatGPTReplies(t, s, session, v.ParticipationID)
	h := v.Coordination.Handoffs[0]
	req := NotificationRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "attempt", ResultID: h.ID, Stage: "notification_attempted", Automatic: true, PanelID: "one"}
	if r = chatGPTCall(t, s, session, "chatgpt.notification", req); r.OK {
		t.Fatal("unregistered panel claimed automatic notification")
	}
	r = chatGPTCall(t, s, session, "chatgpt.notification", NotificationRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "one", Stage: "panel_status", PanelID: "one", Delivery: "enabled"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	if r = chatGPTCall(t, s, session, "chatgpt.notification", req); !r.OK {
		t.Fatal(r.Error)
	}
	req.EventID = "competing"
	if r = chatGPTCall(t, s, session, "chatgpt.notification", req); r.OK {
		t.Fatal("concurrent claim accepted")
	}
	req.EventID = "attempt"
	req.Stage = "host_accepted"
	if r = chatGPTCall(t, s, session, "chatgpt.notification", req); !r.OK {
		t.Fatal(r.Error)
	}
	if !r.Result.(chat.CoordinationView).Handoffs[0].Open() {
		t.Fatal("host acceptance resolved handoff")
	}
}
