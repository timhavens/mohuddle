package room

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestReadOnlyTasksPreserveConfiguredFilesystemScope(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	for _, participant := range []chat.Participant{chat.Codex, "codex-1", chat.Claude, chat.Agy, chat.Copilot} {
		for _, profile := range []chat.PermissionProfile{chat.PermissionFull, chat.PermissionWorkspace, chat.PermissionReadOnly} {
			for _, spec := range []turnSpec{
				{readOnly: true, conversationID: "reply"},
				{readOnly: true, delegated: true},
				{readOnly: true, planOnly: true},
				{readOnly: true, coreParticipants: []chat.Participant{participant}},
			} {
				o.mu.Lock()
				o.settings[participant] = chat.AgentSettings{Permissions: profile}
				o.mu.Unlock()
				request := o.turnRequest(participant, spec, nil)
				if request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 || request.Access.Configured != profile {
					t.Fatalf("%s %s lost task/profile boundary: %+v", participant, profile, request.Access)
				}
				if profile == chat.PermissionFull {
					if request.Access.ReadScope != chat.ReadScopeHost || len(request.ReadRoots) == 0 || strings.Contains(request.SystemPrompt, "If you need a directory outside") {
						t.Fatalf("%s lost full-machine reads: %+v", participant, request.Access)
					}
				} else if request.Access.ReadScope != chat.ReadScopeGranted {
					t.Fatalf("restricted profile widened: %+v", request.Access)
				}
				o.mu.Lock()
				unchanged := o.settings[participant].Permissions == profile
				o.mu.Unlock()
				if !unchanged {
					t.Fatal("saved profile was changed")
				}
			}
		}
	}
}

func TestFullMachineRoutingStillHasNoTools(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	o.settings[chat.Codex] = chat.AgentSettings{Permissions: chat.PermissionFull}
	for _, spec := range []turnSpec{{private: true}, {noTools: true}} {
		request := o.turnRequest(chat.Codex, spec, nil)
		if !request.NoTools || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 || request.Access.ReadScope != chat.ReadScopeNone {
			t.Fatalf("routing gained tools/roots: %+v", request.Access)
		}
	}
}

func TestFullMachineWorkflowCeilingPreservesReads(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	o.settings[chat.Codex] = chat.AgentSettings{Permissions: chat.PermissionFull}
	o.room.Workflows["ceiling"] = chat.WorkflowRecord{PermissionCeiling: chat.PermissionReadOnly}
	request := o.turnRequest(chat.Codex, turnSpec{workflowID: "ceiling", planOnly: true}, nil)
	if request.Access.ReadScope != chat.ReadScopeHost || !request.Access.ReadOnly || request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 || request.NoTools {
		t.Fatalf("workflow ceiling lost read scope or allowed writes: %+v", request.Access)
	}
}

func TestAGYReadOnlyInspectionPreservesWorkerSession(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	o.settings[chat.Agy] = chat.AgentSettings{Permissions: chat.PermissionFull}
	want := chat.AgentSession{ID: "saved-worker", Cursor: 17, PromptHash: "saved-binding"}
	o.room.Sessions[chat.Agy] = want
	request := o.turnRequest(chat.Agy, turnSpec{readOnly: true, planOnly: true}, nil)
	if !request.Ephemeral || persistentTurn(request) {
		t.Fatal("read-only inspection would replace the saved worker session")
	}
	if _, err := o.recordResult(chat.Agy, agent.TurnResult{Text: "Inspection result"}, 20, persistentTurn(request)); err != nil {
		t.Fatal(err)
	}
	state, _ := o.Snapshot()
	if got := state.Sessions[chat.Agy]; got != want {
		t.Fatalf("worker session changed: %+v", got)
	}
	request = o.turnRequest(chat.Agy, turnSpec{}, nil)
	if request.Ephemeral || !persistentTurn(request) {
		t.Fatal("normal writable work lost its persistent session")
	}
}

func TestProviderCancellationIsNotReportedAsDeadline(t *testing.T) {
	o, codex, _ := newTestOrchestrator(t)
	defer o.Close()
	codex.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{}, context.Canceled
	}
	connectChatGPT(o)
	if _, _, err := o.PublishChatGPT("Review", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "provider-cancel"}); err != nil {
		t.Fatal(err)
	}
	jobs := waitChatGPTReplies(t, o, 1)
	if jobs[0].ReasonCode != chat.ReasonProvider || jobs[0].CompletedAt == nil {
		t.Fatalf("provider cancellation mislabeled: %+v", jobs[0])
	}
}

func TestFullMachineReplyCorrectsRedundantReadRequestOnce(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		o, codex, _ := newTestOrchestrator(t)
		o.settings[chat.Codex] = chat.AgentSettings{Permissions: chat.PermissionFull}
		codex.run = func(_ context.Context, call int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
			if request.Access.ReadScope != chat.ReadScopeHost || !request.Access.ReadOnly || len(request.WriteRoots) != 0 {
				t.Error("lost read-only host scope")
			}
			emit(agent.Event{Type: agent.EventDelta, Text: "Partial diagnostic"})
			if call == 1 || repeated {
				return agent.TurnResult{AccessRequest: &agent.AccessRequest{Path: filepath.Join(t.TempDir(), "outside"), Mode: chat.AccessRead}}, nil
			}
			if !strings.Contains(request.Prompt, "HOST PERMISSION CORRECTION") {
				t.Error("missing correction")
			}
			return agent.TurnResult{Text: "Diagnostic completed", Done: true}, nil
		}
		connectChatGPT(o)
		if _, _, err := o.PublishChatGPT("Read the diagnostic", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "read"}); err != nil {
			t.Fatal(err)
		}
		jobs := waitChatGPTReplies(t, o, 1)
		if codex.callCount() != 2 {
			t.Fatalf("calls=%d", codex.callCount())
		}
		if repeated {
			if jobs[0].ReasonCode != chat.ReasonInvalidAccess || !jobs[0].HasPartialResponse {
				t.Fatalf("failed reply=%+v", jobs[0])
			}
		} else if jobs[0].State != chat.ConversationAnswered || jobs[0].AnswerSequence == 0 {
			t.Fatalf("reply=%+v", jobs[0])
		}
		state, _ := o.Snapshot()
		if len(state.Grants) != 1 {
			t.Fatal("redundant access changed persistent grants")
		}
		o.Close()
	}
}

func TestChatGPTLeaseExpiryPreservesRunningAndQueuedReplies(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	started, finish := make(chan struct{}), make(chan struct{})
	codex.run = func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		select {
		case <-finish:
			return agent.TurnResult{Text: "Finished after disconnect", Done: true}, nil
		case <-ctx.Done():
			t.Error("lease cancelled active reply")
			return agent.TurnResult{}, ctx.Err()
		}
	}
	claude.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "Queued reply completed", Done: true}, nil
	}
	connectChatGPT(o)
	// Hold only the test provider's gate; the queued reply must launch after
	// lease expiry without needing ChatGPT to become present again.
	gate := o.agentGates[chat.Claude]
	gate.Lock()
	if _, _, err := o.PublishChatGPT("Review", 0, []chat.Participant{chat.Codex, chat.Claude}, chat.RouteMetadata{MessageID: "lease"}); err != nil {
		gate.Unlock()
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		gate.Unlock()
		t.Fatal("reply did not start")
	}
	o.mu.Lock()
	state := *o.room.ChatGPT
	state.LeaseUntil = time.Now().Add(-time.Second)
	o.mu.Unlock()
	o.UpdateChatGPTState(state)
	o.scheduleConversations()
	snapshot, _ := o.Snapshot()
	if snapshot.ChatGPT.Connected {
		t.Fatal("expired lease still present")
	}
	for _, job := range snapshot.Conversations {
		if job.State.Terminal() {
			t.Fatalf("lease terminated accepted reply: %+v", job)
		}
	}
	gate.Unlock()
	close(finish)
	for _, job := range waitChatGPTReplies(t, o, 2) {
		if job.State != chat.ConversationAnswered || job.CompletedAt == nil {
			t.Fatalf("reply=%+v", job)
		}
	}
	connectChatGPT(o)
	if codex.callCount() != 1 || claude.callCount() != 1 {
		t.Fatal("rejoin duplicated accepted work")
	}
}
