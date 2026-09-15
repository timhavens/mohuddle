package room

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

func TestChatGPTRoundUsesNativeReadOnlyFloorAndKeepsAuthorship(t *testing.T) {
	o, agents := newFourAgentOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	var order []chat.Participant
	for participant, peer := range agents {
		participant := participant
		peer.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			order = append(order, participant)
			if request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 {
				t.Errorf("round granted write access to %s", participant)
			}
			if participant == chat.Codex && (!strings.Contains(request.Prompt, "claude review") || !strings.Contains(request.Prompt, "agy review")) {
				t.Error("moderator did not receive earlier reviews")
			}
			return agent.TurnResult{Text: string(participant) + " review", Done: true}, nil
		}
	}
	route := chat.RouteMetadata{MessageID: "structured-round"}
	selected := []chat.Participant{chat.Codex, chat.Claude, chat.Agy}
	message, created, err := o.RequestChatGPTRound("Review this existing proposal; discussion only", selected, 0, route)
	if err != nil || !created || message.Author != chat.ChatGPT || message.Round == nil {
		t.Fatalf("round submission: %+v created=%t err=%v", message, created, err)
	}
	o.wg.Wait()
	want := []chat.Participant{chat.Claude, chat.Agy, chat.Codex}
	if !slices.Equal(order, want) || !slices.Equal(message.Round.Participants, want) || message.Round.Moderator != chat.Codex || agents[chat.Copilot].callCount() != 0 {
		t.Fatalf("round order=%v metadata=%+v", order, message.Round)
	}
	state, _ := o.Snapshot()
	record := state.Workflows[message.WorkflowID]
	if record.State != chat.WorkflowCompleted || record.Resource != chat.WorkflowReadOnly || record.PermissionCeiling != chat.PermissionReadOnly || record.Mode != state.WorkflowMode.WithDefault() {
		t.Fatal("round did not finish as a read-only workflow")
	}
	duplicate, created, err := o.RequestChatGPTRound(message.Text, selected, 0, route)
	if err != nil || created || duplicate.Sequence != message.Sequence || len(order) != 3 {
		t.Fatal("retry duplicated the round")
	}
	if _, _, err := o.RequestChatGPTRound(message.Text, []chat.Participant{chat.Claude}, 0, route); err == nil {
		t.Fatal("retry changed selected participants")
	}
	if _, _, err := o.RequestChatGPTWork(message.Text, chat.Codex, 0, route); err == nil {
		t.Fatal("round operation changed into writable work")
	}
	if _, _, err := o.PublishChatGPT(message.Text, 0, selected, route); err == nil {
		t.Fatal("round operation changed into independent replies")
	}
}

func TestChatGPTRoundRequiresEarlierOperationsToFinish(t *testing.T) {
	for _, work := range []bool{false, true} {
		t.Run(map[bool]string{false: "peer-draft", true: "work"}[work], func(t *testing.T) {
			o, codex, _ := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			started, release := make(chan struct{}), make(chan struct{})
			codex.run = func(ctx context.Context, call int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
				if call == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return agent.TurnResult{}, ctx.Err()
					}
				}
				return agent.TurnResult{Text: "The completed draft", Done: true}, nil
			}
			var err error
			if work {
				_, _, err = o.RequestChatGPTWork("Prepare the artifact", chat.Codex, 0, chat.RouteMetadata{MessageID: "prepare"})
			} else {
				_, _, err = o.PublishChatGPT("Draft an answer in the room", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "prepare"})
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("first operation did not start")
			}
			_, before := o.Snapshot()
			route := chat.RouteMetadata{MessageID: "review-after-draft"}
			message, created, err := o.RequestChatGPTRound("Review the draft", nil, 0, route)
			_, after := o.Snapshot()
			if err == nil || created || message.Sequence != 0 || len(after) != len(before) {
				t.Fatal("dependent round was posted or dispatched while its input was pending")
			}
			close(release)
			if work {
				o.wg.Wait()
			} else {
				waitChatGPTReplies(t, o, 1)
			}
			if _, created, err := o.RequestChatGPTRound("Review the draft", nil, 0, route); err != nil || !created {
				t.Fatalf("settled operation prevented round: %v", err)
			}
			o.wg.Wait()
		})
	}
}

func TestChatGPTRoundPersistenceFailureCannotRunOrChangeAction(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &controlledStore{base: base}
	peer := &fakeAgent{participant: chat.Codex}
	o, err := New(state, nil, wrapped, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	connectChatGPT(o)
	wrapped.failNextSave()
	route := chat.RouteMetadata{MessageID: "failed-round"}
	message, created, err := o.RequestChatGPTRound("Discuss this proposal", nil, 0, route)
	if err == nil || !created {
		t.Fatal("persistence failure was hidden")
	}
	if err := o.ResumeQueued(); err != nil {
		t.Fatal(err)
	}
	duplicate, created, err := o.RequestChatGPTRound("Discuss this proposal", nil, 0, route)
	if err == nil || created || duplicate.Sequence != message.Sequence || peer.callCount() != 0 {
		t.Fatal("failed round was dispatched or duplicated")
	}
	persisted, err := base.LoadRoom(state.ID)
	if err != nil || persisted.Workflows[message.WorkflowID].State != chat.WorkflowCancelled {
		t.Fatal("failed round state was not retained")
	}
}

func TestChatGPTWorkRunsWithNormalPermissionsAndKeepsAuthorship(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	state, _ := o.Snapshot()
	output := filepath.Join(state.Workspace, "requested-edit.txt")
	task := "Make the requested title-only documentation edit. Preserve unrelated changes. No commit or push."
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral || request.NoTools || request.Settings.Permissions != chat.PermissionWorkspace || len(request.WriteRoots) == 0 {
			t.Error("work request did not receive the normal writable turn")
		}
		if !strings.Contains(request.Prompt, task) || !strings.Contains(request.Prompt, "authorized ChatGPT to assign work") {
			t.Error("delegated task or authority missing from provider prompt")
		}
		if err := os.WriteFile(output, []byte("requested title"), 0o600); err != nil {
			return agent.TurnResult{}, err
		}
		return agent.TurnResult{Text: "Completed the requested title edit", Done: true}, nil
	}
	route := chat.RouteMetadata{MessageID: "work-one"}
	message, created, err := o.RequestChatGPTWork(task, chat.Codex, 0, route)
	if err != nil || !created || message.Author != chat.ChatGPT || !message.IsWorkflowSource() || message.Target != chat.Codex {
		t.Fatalf("work submission: %+v, created=%t, err=%v", message, created, err)
	}
	o.wg.Wait()
	if text, err := os.ReadFile(output); err != nil || string(text) != "requested title" {
		t.Fatalf("requested edit missing: %v", err)
	}
	state, messages := o.Snapshot()
	if state.Workflows[message.WorkflowID].State != chat.WorkflowCompleted || len(state.Conversations) != 0 || claude.callCount() != 0 {
		t.Fatal("assignment did not run as one direct workflow")
	}
	found := false
	for _, reply := range messages {
		if reply.Author == chat.Codex && reply.WorkflowID == message.WorkflowID && strings.Contains(reply.Text, "Completed") {
			found = true
		}
	}
	if !found {
		t.Fatal("work result was not published to the shared room")
	}
	duplicate, created, err := o.RequestChatGPTWork(task, chat.Codex, 0, route)
	if err != nil || created || duplicate.Sequence != message.Sequence || codex.callCount() != 1 {
		t.Fatal("retry duplicated work")
	}
	if _, _, err := o.RequestChatGPTWork(task+" changed", chat.Codex, 0, route); err == nil {
		t.Fatal("operation accepted changed task")
	}
	if _, _, err := o.PublishChatGPT(task, 0, nil, route); err == nil {
		t.Fatal("work operation was reused for a different action")
	}
}

func TestChatGPTWorkQueuesBehindWriterAndPreservesPermissionCeiling(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	started, release := make(chan struct{}), make(chan struct{})
	claude.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agent.TurnResult{Text: "Human's existing work complete", Done: true}, nil
	}
	if err := o.Post("@claude Implement the existing human task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("existing writer did not start")
	}
	message, _, err := o.RequestChatGPTWork("Implement the next task", chat.Codex, 0, chat.RouteMetadata{MessageID: "queued-work"})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := o.Snapshot()
	if state.Workflows[message.WorkflowID].State != chat.WorkflowWaiting || len(state.PendingInputs) != 1 || codex.callCount() != 0 {
		t.Fatal("ChatGPT bypassed the workspace writer queue")
	}
	o.mu.Lock()
	settings := o.settings[chat.Codex]
	settings.Permissions = chat.PermissionFull
	o.settings[chat.Codex] = settings
	o.mu.Unlock()
	close(release)
	o.wg.Wait()
	if codex.callCount() != 1 || codex.request(0).Settings.Permissions != chat.PermissionWorkspace {
		t.Fatal("queued work failed to run with its original permission ceiling")
	}
}

func TestChatGPTWorkHonorsReadOnlyAndPlanMode(t *testing.T) {
	for _, plan := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only-permission", true: "plan-mode"}[plan], func(t *testing.T) {
			o, codex, _ := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			o.mu.Lock()
			if plan {
				o.room.WorkflowMode = chat.WorkflowPlan
			} else {
				settings := o.settings[chat.Codex]
				settings.Permissions = chat.PermissionReadOnly
				o.settings[chat.Codex] = settings
			}
			o.mu.Unlock()
			codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
				if request.Settings.Permissions != chat.PermissionReadOnly {
					t.Error("work elevated read-only permission")
				}
				return agent.TurnResult{Text: "<proposed_plan>Make the requested edit after approval.</proposed_plan>", Done: true}, nil
			}
			if _, _, err := o.RequestChatGPTWork("Make an edit", chat.Codex, 0, chat.RouteMetadata{MessageID: "read-only-work"}); err != nil {
				t.Fatal(err)
			}
			o.wg.Wait()
			if codex.callCount() == 0 {
				t.Fatal("work was not dispatched")
			}
		})
	}
}

func TestChatGPTWorkQueueIsBoundedAndHostStopCancelsIt(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	started := make(chan struct{})
	claude.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}
	if err := o.Post("@claude Implement the existing task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if _, _, err := o.RequestChatGPTWork("Make the requested edit", chat.Codex, 0, chat.RouteMetadata{MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := o.RequestChatGPTWork("Make another edit", chat.Codex, 0, chat.RouteMetadata{MessageID: "five"}); err == nil {
		t.Fatal("pending work limit bypassed")
	}
	o.Stop()
	o.wg.Wait()
	state, _ := o.Snapshot()
	if len(state.PendingInputs) != 0 || codex.callCount() != 0 || !state.ChatGPT.Paused {
		t.Fatal("host stop failed to cancel queued work")
	}
	if _, _, err := o.RequestChatGPTWork("Start more work", chat.Codex, 0, chat.RouteMetadata{MessageID: "after-stop"}); err == nil {
		t.Fatal("paused ChatGPT scheduled more work")
	}
}

func TestChatGPTWorkUsesNormalHumanApprovalFlow(t *testing.T) {
	o, codex, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	stream, stop := o.SubscribeEvents(64)
	defer stop()
	decision := make(chan agent.ApprovalDecision)
	codex.run = func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		emit(agent.Event{Type: agent.EventApproval, Approval: &agent.ApprovalRequest{Title: "Approve work action", Response: decision}})
		select {
		case result := <-decision:
			if result != agent.Deny {
				t.Error("unexpected approval")
			}
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{Text: "Action declined", Done: true}, nil
	}
	if _, _, err := o.RequestChatGPTWork("Perform the approved task", chat.Codex, 0, chat.RouteMetadata{MessageID: "approval-work"}); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-stream:
			if event.AgentEvent != nil && event.AgentEvent.Type == agent.EventApproval {
				event.AgentEvent.Approval.Response <- agent.Deny
				o.wg.Wait()
				return
			}
		case <-timer.C:
			t.Fatal("work approval did not reach the human UI")
		}
	}
}

func TestChatGPTWorkPersistenceFailureNeverDispatchesOnRetry(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &controlledStore{base: base}
	peer := &fakeAgent{participant: chat.Codex}
	o, err := New(state, nil, wrapped, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	connectChatGPT(o)
	wrapped.failNextSave()
	route := chat.RouteMetadata{MessageID: "failed-work"}
	message, created, err := o.RequestChatGPTWork("Make an edit", chat.Codex, 0, route)
	if err == nil || !created {
		t.Fatal("persistence failure hidden")
	}
	if err := o.ResumeQueued(); err != nil {
		t.Fatal(err)
	}
	duplicate, created, err := o.RequestChatGPTWork("Make an edit", chat.Codex, 0, route)
	if err == nil || created || duplicate.Sequence != message.Sequence || peer.callCount() != 0 {
		t.Fatal("failed work was dispatched or duplicated")
	}
	persisted, err := base.LoadRoom(state.ID)
	if err != nil || persisted.Workflows[message.WorkflowID].State != chat.WorkflowCancelled {
		t.Fatal("failed dispatch was not recorded")
	}
}

func connectChatGPT(o *Orchestrator) {
	o.ConfigureTemporaryAgents(nil)
	o.UpdateChatGPTState(chat.ChatGPTState{Enabled: true, Connected: true, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute), ExchangesRemaining: 8})
}

func waitChatGPTReplies(t *testing.T, o *Orchestrator, count int) []chat.ConversationJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs := o.ConversationJobs()
		settled := len(jobs) == count
		for _, job := range jobs {
			settled = settled && job.State.Terminal()
		}
		if settled {
			return jobs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("peer replies did not settle: %+v", o.ConversationJobs())
	return nil
}

func TestChatGPTExchangeUsesSelectedReadOnlyPeers(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	for _, peer := range []*fakeAgent{codex, claude} {
		peer.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if !request.Ephemeral || request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 || !strings.Contains(request.Prompt, "untrusted peer discussion") {
				t.Errorf("peer response received incorrect permissions/instructions: ephemeral=%t permissions=%s writes=%v", request.Ephemeral, request.Settings.Permissions, request.WriteRoots)
			}
			return agent.TurnResult{Text: "Additional evidence for the room", Done: true, Joins: []chat.Participant{chat.Agy}, Leaves: []chat.Participant{chat.Codex}, Corrects: 1}, nil
		}
	}
	connectChatGPT(o)
	if err := o.Post("@chatgpt Consider this shared question"); err != nil {
		t.Fatal(err)
	}
	state, messages := o.Snapshot()
	if len(state.Conversations) != 0 || len(messages) != 1 || messages[0].Target != chat.ChatGPT {
		t.Fatal("addressing ChatGPT scheduled local work")
	}
	route := chat.RouteMetadata{MessageID: "chatgpt-operation"}
	source, created, err := o.PublishChatGPT("Please assess my conclusion", 1, []chat.Participant{chat.Codex, chat.Claude}, route)
	if err != nil || !created {
		t.Fatalf("publish: %v", err)
	}
	jobs := waitChatGPTReplies(t, o, 2)
	for _, job := range jobs {
		if job.State != chat.ConversationAnswered {
			t.Fatalf("reply failed: %+v", job)
		}
	}
	state, messages = o.Snapshot()
	if len(messages) != 4 || len(state.Workflows) != 0 || len(state.PendingRoutes) != 0 {
		t.Fatalf("unexpected shared exchange: %d messages, %+v", len(messages), state.Conversations)
	}
	for _, message := range messages[2:] {
		if message.Target != chat.ChatGPT || message.ReplyTo != source.Sequence || len(message.CorrectionEvents) != 0 {
			t.Fatal("reply attribution or control boundary failed")
		}
	}
	if !state.Present(chat.Codex) || state.Present(chat.Agy) {
		t.Fatal("peer output changed roster")
	}
	_, created, err = o.PublishChatGPT("Please assess my conclusion", 1, []chat.Participant{chat.Codex, chat.Claude}, route)
	if err != nil || created || codex.callCount() != 1 || claude.callCount() != 1 {
		t.Fatal("retry duplicated messages or peer requests")
	}
	if err := o.PromoteConversation(jobs[0].ID, false); err == nil {
		t.Fatal("AI source was promoted into human work")
	}
}

func TestChatGPTPeerCannotRequestWorkOrAlternateProvider(t *testing.T) {
	for _, requiresWork := range []bool{true, false} {
		t.Run(map[bool]string{true: "work", false: "provider-failure"}[requiresWork], func(t *testing.T) {
			o, codex, claude := newTestOrchestrator(t)
			defer o.Close()
			codex.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
				if requiresWork {
					return agent.TurnResult{Text: "I need write access", RequiresWork: true}, nil
				}
				return agent.TurnResult{}, errors.New("provider unavailable")
			}
			connectChatGPT(o)
			if _, _, err := o.PublishChatGPT("Implement my suggestion", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "operation"}); err != nil {
				t.Fatal(err)
			}
			jobs := waitChatGPTReplies(t, o, 1)
			state, _ := o.Snapshot()
			if jobs[0].State != chat.ConversationFailed || len(state.PendingRoutes) != 0 || len(state.Workflows) != 0 || claude.callCount() != 0 {
				t.Fatal("external request escaped bounded conversation")
			}
		})
	}
}

func TestChatGPTRevocationAndLeaseExpiryCancelPeerReplies(t *testing.T) {
	for _, reason := range []string{"revoke", "expiry", "stop"} {
		t.Run(reason, func(t *testing.T) {
			o, codex, _ := newTestOrchestrator(t)
			defer o.Close()
			started, stopped := make(chan struct{}), make(chan struct{})
			codex.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return agent.TurnResult{Text: "late reply must not publish", Done: true}, nil
			}
			connectChatGPT(o)
			if _, _, err := o.PublishChatGPT("Please review", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "operation"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("reply did not start")
			}
			switch reason {
			case "revoke":
				o.UpdateChatGPTState(chat.ChatGPTState{})
			case "stop":
				o.Stop()
			case "expiry":
				o.mu.Lock()
				o.room.ChatGPT.LeaseUntil = time.Now().Add(-time.Second)
				o.mu.Unlock()
				o.scheduleConversations()
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("peer turn ignored cancellation")
			}
			o.wg.Wait()
			jobs := waitChatGPTReplies(t, o, 1)
			_, messages := o.Snapshot()
			if jobs[0].State != chat.ConversationCancelled || len(messages) != 1 {
				t.Fatal("late reply survived cancellation")
			}
		})
	}
}

func TestChatGPTFailedPersistenceCannotDispatchOrDuplicateReplies(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &controlledStore{base: base}
	peer := &fakeAgent{participant: chat.Codex}
	o, err := New(state, nil, wrapped, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	connectChatGPT(o)
	wrapped.failNextSave()
	route := chat.RouteMetadata{MessageID: "failed-operation"}
	message, created, err := o.PublishChatGPT("Please review", 0, []chat.Participant{chat.Codex}, route)
	if err == nil || !created {
		t.Fatal("partial persistence failure was hidden")
	}
	o.scheduleConversations()
	if peer.callCount() != 0 {
		t.Fatal("peer ran before durable authorization")
	}
	duplicate, created, err := o.PublishChatGPT("Please review", 0, []chat.Participant{chat.Codex}, route)
	if err == nil || created || duplicate.Sequence != message.Sequence {
		t.Fatal("retry hid failed dispatch or duplicated contribution")
	}
	_, messages := o.Snapshot()
	if len(messages) != 1 {
		t.Fatal("retry added a second contribution")
	}
	persisted, err := base.LoadRoom(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ChatGPT != nil || persisted.Conversations[0].State != chat.ConversationFailed {
		t.Fatal("grant persisted or failure was not saved")
	}
}

func TestChatGPTPeerApprovalIsDeniedWithoutPromptingHuman(t *testing.T) {
	o, codex, _ := newTestOrchestrator(t)
	defer o.Close()
	stream, stop := o.SubscribeEvents(64)
	defer stop()
	codex.run = func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		decision := make(chan agent.ApprovalDecision)
		emit(agent.Event{Type: agent.EventApproval, Approval: &agent.ApprovalRequest{Title: "Escalate peer access", Response: decision}})
		select {
		case value := <-decision:
			if value != agent.Deny {
				t.Error("external peer approval was accepted")
			}
		case <-time.After(time.Second):
			t.Error("external peer approval was not answered")
		case <-ctx.Done():
			t.Error("peer turn ended before automatic denial")
		}
		return agent.TurnResult{Text: "I can discuss without additional access", Done: true}, nil
	}
	connectChatGPT(o)
	if _, _, err := o.PublishChatGPT("Please review", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "approval-attempt"}); err != nil {
		t.Fatal(err)
	}
	waitChatGPTReplies(t, o, 1)
	for {
		select {
		case event := <-stream:
			if event.AgentEvent != nil && event.AgentEvent.Type == agent.EventApproval {
				t.Fatal("peer approval reached human UI")
			}
		default:
			return
		}
	}
}
